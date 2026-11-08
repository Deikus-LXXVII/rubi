package agentsandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) (Policy, string) {
	t.Helper()
	root := t.TempDir()
	dir := func(p string) string {
		v := filepath.Join(root, p)
		if err := os.MkdirAll(v, 0700); err != nil {
			t.Fatal(err)
		}
		return v
	}
	p := Policy{Home: dir("home"), Projects: []string{dir("home/p1")}, Login: dir("home/login"), Tmp: dir("tmp"), RuntimeRead: []string{dir("runtime")}, AllowedDomains: []string{"github.com"}}
	return p, dir("settings")
}
func TestBuild(t *testing.T) {
	p, settings := fixture(t)
	s, e := Build(p, []string{settings})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(s.Filesystem.AllowWrite, []string{p.Projects[0], p.Login, p.Tmp}) {
		t.Fatal(s)
	}
	if !reflect.DeepEqual(s.Filesystem.AllowRead, []string{p.Projects[0], p.Login, p.RuntimeRead[0]}) {
		t.Fatal(s)
	}
	if !reflect.DeepEqual(s.Network.AllowedDomains, []string{"github.com"}) {
		t.Fatal(s)
	}
	for _, root := range []string{p.Home, "/workspace", "/cursor"} {
		if _, e := os.Stat(root); e == nil && !contains(s.Filesystem.DenyRead, root) {
			t.Fatalf("not hidden: %s", root)
		}
	}
}
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func TestRejectUnsafePolicies(t *testing.T) {
	for _, name := range []string{"read ancestor", "read tmp", "nested tmp", "settings writable", "runtime writable", "home writable", "relative", "symlink", "domain"} {
		t.Run(name, func(t *testing.T) {
			p, settings := fixture(t)
			protected := []string{settings}
			switch name {
			case "read ancestor":
				p.RuntimeRead = append(p.RuntimeRead, p.Home)
			case "read tmp":
				p.RuntimeRead = append(p.RuntimeRead, p.Tmp)
			case "nested tmp":
				p.Tmp = p.Projects[0]
			case "settings writable":
				protected = []string{p.Projects[0]}
			case "runtime writable":
				p.RuntimeRead = []string{p.Login}
			case "home writable":
				p.Projects = []string{p.Home}
			case "relative":
				p.Login = "relative"
			case "symlink":
				link := filepath.Join(settings, "link")
				if e := os.Symlink(p.Projects[0], link); e != nil {
					t.Fatal(e)
				}
				protected = []string{link}
			case "domain":
				p.AllowedDomains = []string{"*"}
			}
			if _, e := Build(p, protected); e == nil {
				t.Fatal("accepted unsafe policy")
			}
		})
	}
}
func TestEnvironment(t *testing.T) {
	got := environment([]string{"LANG=C.UTF-8", "API_KEY=fake", "NODE_OPTIONS=bad", "LD_PRELOAD=bad", "PATH=/bad", "HOME=/bad", "HTTP_PROXY=bad", "SSH_AUTH_SOCK=bad", "BASH_ENV=bad"}, "/home/test", "/tmp/private", "/usr/bin")
	want := []string{"HOME=/home/test", "TMPDIR=/tmp/private", "PATH=/usr/bin", "SHELL=/bin/bash", "LANG=C.UTF-8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("unexpected environment entries")
	}
}
func TestFailClosed(t *testing.T) {
	p, settings := fixture(t)
	for _, mode := range []Mode{Unsandboxed, 42} {
		_, e := Run(context.Background(), Launch{Mode: mode}, nil, nil, nil)
		if e == nil {
			t.Fatal("mode bypass")
		}
	}
	_, e := Run(context.Background(), Launch{Policy: p, SettingsDir: settings, SRT: filepath.Join(settings, "missing")}, nil, nil, nil)
	if e == nil || !strings.Contains(e.Error(), "srt missing") {
		t.Fatal(e)
	}
	_, e = find("socat", []string{settings})
	if e == nil || !strings.Contains(e.Error(), "socat missing") {
		t.Fatal(e)
	}
}
func TestFakeRuntimeArgvStdioExit(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Unix runtime adapter")
	}
	p, settings := fixture(t)
	runtimeDir := p.RuntimeRead[0]
	for _, dep := range []string{"node", "rg", "bwrap", "socat", "sandbox-exec"} {
		if e := os.WriteFile(filepath.Join(runtimeDir, dep), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
			t.Fatal(e)
		}
	}
	srt := filepath.Join(runtimeDir, "srt")
	script := "#!/bin/sh\n[ \"$1\" = --settings ] || exit 90\n[ -f \"$2\" ] || exit 91\n[ \"$3\" = -- ] || exit 92\nshift 4\n[ \"$1\" = 'a b;$(false)' ] || exit 93\ncat\nprintf 'stderr' >&2\nexit 17\n"
	if e := os.WriteFile(srt, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	l := Launch{Policy: p, SettingsDir: settings, SRT: srt, CLI: "/bin/true", SearchPath: []string{runtimeDir, "/usr/bin", "/bin"}, Args: []string{"a b;$(false)"}}
	var out, errout bytes.Buffer
	code, e := Run(context.Background(), l, strings.NewReader("{\"ok\":true}\n"), &out, &errout)
	if e != nil || code != 17 || out.String() != "{\"ok\":true}\n" || errout.String() != "stderr" {
		t.Fatalf("code=%d error=%v out=%q err=%q", code, e, out.String(), errout.String())
	}
	entries, e := os.ReadDir(settings)
	if e != nil || len(entries) != 0 {
		t.Fatal("settings not cleaned up")
	}
}

func TestMissingDependencies(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported platform")
	}
	deps := []string{"node", "rg", "bwrap", "socat"}
	if runtime.GOOS == "darwin" {
		deps = []string{"node", "rg", "sandbox-exec"}
	}
	for _, missing := range deps {
		t.Run(missing, func(t *testing.T) {
			p, settings := fixture(t)
			dir := p.RuntimeRead[0]
			for _, name := range append([]string{"srt", "cli"}, deps...) {
				if name != missing {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 99\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := Run(context.Background(), Launch{Policy: p, SettingsDir: settings, SRT: filepath.Join(dir, "srt"), CLI: filepath.Join(dir, "cli"), SearchPath: []string{dir}}, nil, nil, nil)
			if err == nil || !strings.Contains(err.Error(), missing+" missing") {
				t.Fatalf("did not fail closed for %s: %v", missing, err)
			}
			files, err := os.ReadDir(settings)
			if err != nil || len(files) != 0 {
				t.Fatal("preflight wrote settings")
			}
		})
	}
}
