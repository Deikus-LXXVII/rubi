//go:build unix

package homestart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeRun struct {
	calls [][]string // bin, args...
	env   []string
	out   string
	err   error
}

func (f *fakeRun) run(_ context.Context, bin string, args, env []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{bin}, args...))
	f.env = env
	return []byte(f.out), f.err
}

// setup makes a home with an installed, set-up Rubi Home and returns the env and the binary's path.
func setup(t *testing.T, vars map[string]string) (Env, *fakeRun, string) {
	t.Helper()
	home := t.TempDir()
	f := &fakeRun{}
	e := Env{Home: home, UID: os.Getuid(), Run: f.run, Getenv: func(k string) string { return vars[k] }}
	bin := e.Binary()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := e.configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e, f, bin
}

func TestEnsureStartsTheInstalledRubiHome(t *testing.T) {
	e, f, bin := setup(t, map[string]string{"TMPDIR": "/bot", "BOT_SECRET": "x", "LANG": "C.UTF-8", "XDG_RUNTIME_DIR": "/run/x"})
	if err := Ensure(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !slices.Equal(f.calls[0], []string{bin, "start"}) {
		t.Fatalf("ran %q", f.calls)
	}
	if bin != filepath.Join(e.Home, ".rubi-home", "bin", "rubi-home") {
		t.Errorf("binary %s; want the installer's default", bin)
	}
	joined := strings.Join(f.env, "\n")
	for _, want := range []string{"HOME=" + e.Home, "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "XDG_RUNTIME_DIR=/run/x"} {
		if !slices.Contains(f.env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if strings.Contains(joined, "BOT_SECRET") || strings.Contains(joined, "TMPDIR") {
		t.Error("the daemon's environment reached Rubi Home")
	}
}

func TestEnsureIgnoresPathsFromTheEnvironment(t *testing.T) {
	// The daemon's environment is a Bot session's: it can't point the daemon at another binary or folder.
	other := t.TempDir()
	vars := map[string]string{"RUBI_HOME_INSTALL": other, "RUBI_HOME_DIR": other, "XDG_CONFIG_HOME": other, "HOME": other}
	e, f, bin := setup(t, vars)
	_ = os.MkdirAll(filepath.Join(other, "bin"), 0o755)
	_ = os.WriteFile(filepath.Join(other, "bin", "rubi-home"), []byte("#!/bin/sh\n"), 0o755)
	if err := Ensure(context.Background(), e); err != nil || len(f.calls) != 1 || f.calls[0][0] != bin {
		t.Fatalf("Ensure = %v, ran %q; want only %s", err, f.calls, bin)
	}
	for _, kv := range f.env {
		if strings.Contains(kv, other) {
			t.Errorf("%s reached Rubi Home", kv)
		}
	}
}

func TestEnsureChecksEveryFolderAbove(t *testing.T) {
	t.Run("group-writable home", func(t *testing.T) {
		e, f, _ := setup(t, nil)
		_ = os.Chmod(e.Home, 0o775)
		if err := Ensure(context.Background(), e); err == nil || len(f.calls) != 0 {
			t.Errorf("Ensure = %v with %d runs; want a refusal", err, len(f.calls))
		}
	})
	t.Run("install folder through a symlink", func(t *testing.T) {
		e, f, bin := setup(t, nil)
		inst := filepath.Dir(filepath.Dir(bin))
		moved := filepath.Join(t.TempDir(), "inst")
		if err := os.Rename(inst, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, inst); err != nil {
			t.Fatal(err)
		}
		if err := Ensure(context.Background(), e); err != nil || len(f.calls) != 1 {
			t.Errorf("a symlink to a folder only this user can write: %v, %d runs", err, len(f.calls))
		}
		_ = os.Chmod(filepath.Dir(moved), 0o777)
		t.Cleanup(func() { _ = os.Chmod(filepath.Dir(moved), 0o755) })
		f.calls = nil
		if err := Ensure(context.Background(), e); err == nil || len(f.calls) != 0 {
			t.Errorf("a symlink into a folder others can write: %v, %d runs", err, len(f.calls))
		}
	})
}

func TestEnsureNothingInstalled(t *testing.T) {
	e, f, bin := setup(t, nil)
	dir := e.configDir()
	_ = os.Remove(filepath.Join(dir, "identity.json"))
	if err := Ensure(context.Background(), e); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("not set up: %v", err)
	}
	e2, f2, bin2 := setup(t, nil)
	_ = os.Remove(bin2)
	if err := Ensure(context.Background(), e2); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("no binary: %v", err)
	}
	if len(f.calls)+len(f2.calls) != 0 {
		t.Error("ran something with nothing installed")
	}
	_ = bin
}

func TestEnsureRunsOnlyWhatOnlyThisUserCouldWrite(t *testing.T) {
	cases := map[string]func(t *testing.T, bin string){
		"group-writable binary": func(t *testing.T, bin string) { _ = os.Chmod(bin, 0o775) },
		"world-writable binary": func(t *testing.T, bin string) { _ = os.Chmod(bin, 0o757) },
		"writable folder":       func(t *testing.T, bin string) { _ = os.Chmod(filepath.Dir(bin), 0o777) },
		"symlink into a folder others can write": func(t *testing.T, bin string) {
			open := filepath.Join(filepath.Dir(bin), "open")
			_ = os.Mkdir(open, 0o755)
			_ = os.Chmod(open, 0o777)
			_ = os.Rename(bin, filepath.Join(open, "rubi-home"))
			_ = os.Symlink(filepath.Join(open, "rubi-home"), bin)
		},
		"folder": func(t *testing.T, bin string) { _ = os.Remove(bin); _ = os.Mkdir(bin, 0o755) },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			e, f, bin := setup(t, nil)
			spoil(t, bin)
			t.Cleanup(func() { _ = os.Chmod(filepath.Dir(bin), 0o755) })
			if err := Ensure(context.Background(), e); err == nil || errors.Is(err, ErrNotInstalled) {
				t.Errorf("Ensure = %v; want a refusal", err)
			}
			if len(f.calls) != 0 {
				t.Error("ran an untrusted binary")
			}
		})
	}
	t.Run("another user's", func(t *testing.T) {
		e, f, _ := setup(t, nil)
		e.UID++
		if err := Ensure(context.Background(), e); err == nil || len(f.calls) != 0 {
			t.Errorf("Ensure = %v with %d runs; want a refusal", err, len(f.calls))
		}
	})
}

func TestEnsureReportsAFailedStart(t *testing.T) {
	e, f, _ := setup(t, nil)
	f.out, f.err = "the supervisor ended at once", errors.New("exit status 1")
	if err := Ensure(context.Background(), e); err == nil || !strings.Contains(err.Error(), "ended at once") {
		t.Errorf("Ensure = %v", err)
	}
}

func TestKeepLogsOnce(t *testing.T) {
	e, f, _ := setup(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var logs []string
	f2 := f.run
	e.Run = func(c context.Context, bin string, args, env []string) ([]byte, error) {
		defer cancel() // one round
		return f2(c, bin, args, env)
	}
	Keep(ctx, e, func(format string, a ...any) { logs = append(logs, format) })
	if len(f.calls) != 1 || len(logs) != 1 {
		t.Errorf("runs %d, logs %q", len(f.calls), logs)
	}
	// Nothing installed is not worth a log line.
	e2 := Env{Home: t.TempDir(), UID: os.Getuid(), Getenv: func(string) string { return "" }}
	_ = e2.Binary()
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	logs = nil
	Keep(ctx2, e2, func(format string, a ...any) { logs = append(logs, format) })
	if len(logs) != 0 {
		t.Errorf("logged %q with nothing installed", logs)
	}
}
