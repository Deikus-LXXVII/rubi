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
	dir, _ := e.configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return e, f, bin
}

func TestEnsureStartsTheInstalledRubiHome(t *testing.T) {
	e, f, bin := setup(t, map[string]string{"USER": "u", "RUBI_HOME_DIR": "", "BOT_SECRET": "x", "LANG": "C.UTF-8"})
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
	for _, want := range []string{"HOME=" + e.Home, "PATH=/usr/local/bin:/usr/bin:/bin", "USER=u", "LANG=C.UTF-8"} {
		if !slices.Contains(f.env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
	if strings.Contains(joined, "BOT_SECRET") {
		t.Error("the daemon's environment reached Rubi Home")
	}
}

func TestEnsureFollowsTheInstallAndConfigFolders(t *testing.T) {
	root := t.TempDir()
	vars := map[string]string{"RUBI_HOME_INSTALL": filepath.Join(root, "inst"), "XDG_CONFIG_HOME": filepath.Join(root, "cfg")}
	e, f, bin := setup(t, vars)
	if bin != filepath.Join(root, "inst", "bin", "rubi-home") {
		t.Errorf("binary %s", bin)
	}
	if _, err := os.Stat(filepath.Join(root, "cfg", "Rubi Home", "identity.json")); err != nil {
		t.Errorf("config folder: %v", err)
	}
	if err := Ensure(context.Background(), e); err != nil || len(f.calls) != 1 {
		t.Errorf("Ensure = %v, %d runs", err, len(f.calls))
	}
	if !slices.Contains(f.env, "XDG_CONFIG_HOME="+vars["XDG_CONFIG_HOME"]) {
		t.Error("Rubi Home wouldn't find its folder")
	}
	vars["RUBI_HOME_DIR"] = filepath.Join(root, "elsewhere")
	if err := Ensure(context.Background(), e); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("RUBI_HOME_DIR with no Rubi Home in it: %v", err)
	}
}

func TestEnsureNothingInstalled(t *testing.T) {
	e, f, bin := setup(t, nil)
	dir, _ := e.configDir()
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
		"symlink": func(t *testing.T, bin string) {
			real := bin + ".real"
			_ = os.Rename(bin, real)
			_ = os.Symlink(real, bin)
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
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	logs = nil
	Keep(ctx2, e2, func(format string, a ...any) { logs = append(logs, format) })
	if len(logs) != 0 {
		t.Errorf("logged %q with nothing installed", logs)
	}
}
