package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--git-credential-helper" {
		main()
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "--bridge-fake" {
		fakeBridgeService()
		return
	}
	if os.Getenv("RUBI_AGENT_EXEC_TEST") == "1" || (len(os.Args) == 2 && os.Args[1] == "--bridge-relay") {
		main()
		return
	}
	os.Exit(m.Run())
}

// The same executable entrypoint T3 calls, exercised through the generated shim,
// with a disposable Rubi config and a fake provider (no provider account or daemon).
func TestBoundShimSRTIntegration(t *testing.T) {
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt unavailable")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("unsupported platform")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "rubi")
	a := filepath.Join(root, "project-a")
	b := filepath.Join(root, "project-b")
	for _, p := range []string{dir, a, b} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, v any) {
		t.Helper()
		os.MkdirAll(filepath.Dir(path), 0700)
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	self, _ := os.Executable()
	var search, reads []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(d) {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				search = append(search, d)
				if strings.HasPrefix(d, "/workspace/") {
					reads = append(reads, d)
				}
			}
		}
	}
	real, err := filepath.EvalSymlinks(srt)
	if err != nil {
		t.Fatal(err)
	}
	reads = append(reads, filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real)))))
	install := agentsandbox.Installation{AgentExec: self, SRT: srt, SearchPath: search, RuntimeRead: reads, Providers: map[string]agentsandbox.Provider{"claudeAgent": {CLI: python}}}
	config := filepath.Join(dir, "config.json")
	write(config, map[string]any{"agent_projects": []string{a, b}, "agent_sandbox": install})
	r := roles.Role{ID: "r_aaaaaa01", Project: "project-a", DirHash: roles.DirHash(a), Status: "active", Version: 1}
	roleDir := filepath.Join(dir, "agent-roles", r.ID)
	write(filepath.Join(roleDir, "index.json"), r)
	write(filepath.Join(roleDir, "versions", "1.json"), r)
	o := t3vm.InstanceOptions{Dir: dir, T3Base: filepath.Join(root, "t3"), Projects: []t3vm.T3Project{{ID: "a", Root: a}, {ID: "b", Root: b}}}
	_, err = t3vm.SyncSandboxInstances(o, self, []t3vm.SandboxRole{{ID: r.ID, Project: r.Project, ProjectDir: a, Name: "Test", ClaudeDir: filepath.Join(roleDir, "claude")}})
	if err != nil {
		t.Fatal(err)
	}
	shim := o.ShimPath(r.ID)
	if err = os.WriteFile(filepath.Join(b, "marker"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	probe := `import os,sys
project,other,shim,config=sys.argv[1:]
assert os.getcwd()==project
assert os.environ['CLAUDE_CONFIG_DIR'].endswith('/claude')
assert 'RUBI_UNTRUSTED' not in os.environ
open(project+'/written','w').write('ok')
try: open(other+'/marker').read()
except (FileNotFoundError,PermissionError): pass
else: raise AssertionError('other project readable')
for p in [shim,config]:
 assert not os.path.exists(p), 'trusted launch metadata visible'
 # srt may permit recreating a hidden name in a disposable tmpfs. It must
 # never modify the real host shim/config (verified by the parent below).
 try: open(p,'w').write('sandbox-shadow')
 except OSError: pass
sys.stdout.write(sys.stdin.read())
sys.exit(17)
`
	hostShim, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	hostConfig, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shim, "-c", probe, a, b, shim, config)
	cmd.Dir = b // Neither T3's ambient cwd nor its environment can switch project A to B.
	cmd.Env = append(os.Environ(), "RUBI_AGENT_EXEC_TEST=1", "RUBI_UNTRUSTED=fake")
	cmd.Stdin = strings.NewReader("{\"protocol\":true}\n")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 17 || out.String() != "{\"protocol\":true}\n" || stderr.Len() != 0 {
		t.Fatalf("exit=%v stdout=%q stderr=%q", err, out.String(), stderr.String())
	}
	if _, err = os.Stat(filepath.Join(a, "written")); err != nil {
		t.Fatal("project write did not persist", err)
	}
	for path, want := range map[string][]byte{shim: hostShim, config: hostConfig} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("agent modified host launch metadata", err)
		}
	}
	// A previously generated shim must fail after its project is removed.
	write(config, map[string]any{"agent_projects": []string{b}, "agent_sandbox": install})
	cmd = exec.CommandContext(ctx, shim, "--version")
	cmd.Env = append(os.Environ(), "RUBI_AGENT_EXEC_TEST=1")
	data, err := cmd.CombinedOutput()
	if !errors.As(err, &exit) || exit.ExitCode() != 125 || !strings.Contains(string(data), "project") {
		t.Fatalf("removed project launched: %v %q", err, data)
	}
}

// CLI installation and provider arguments must not turn an unconfined SDK/HTTP
// route into a runnable Rubi-managed session. Exercise the actual entrypoint.
func TestUnroutedProvidersRefuseBeforeExecution(t *testing.T) {
	for _, tc := range []struct{ driver, reason string }{
		{"cursor", "shared T3 server"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			for _, missing := range []bool{false, true} {
				t.Run(map[bool]string{false: "configured_project", true: "missing_project"}[missing], func(t *testing.T) {
					root := t.TempDir()
					homeDir := filepath.Join(root, "rubi")
					a, b := filepath.Join(root, "project-a"), filepath.Join(root, "project-b")
					for _, d := range []string{homeDir, a, b} {
						if err := os.MkdirAll(d, 0700); err != nil {
							t.Fatal(err)
						}
					}
					write := func(path string, v any) {
						t.Helper()
						if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
						data, err := json.Marshal(v)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, data, 0600); err != nil {
							t.Fatal(err)
						}
					}
					marker := filepath.Join(root, "must-not-run")
					fakeCLI := filepath.Join(root, "fake-provider")
					if err := os.WriteFile(fakeCLI, []byte("#!/bin/sh\nprintf launched > '"+marker+"'\nexit 97\n"), 0700); err != nil {
						t.Fatal(err)
					}
					projects := []string{a, b}
					if missing {
						projects = []string{b}
					}
					install := agentsandbox.Installation{SRT: fakeCLI, Providers: map[string]agentsandbox.Provider{tc.driver: {CLI: fakeCLI}}}
					write(filepath.Join(homeDir, "config.json"), map[string]any{"agent_projects": projects, "agent_sandbox": install})
					role := roles.Role{ID: "r_aaaaaa01", Project: "project-a", DirHash: roles.DirHash(a), Status: "active", Version: 1}
					roleDir := filepath.Join(homeDir, "agent-roles", role.ID)
					write(filepath.Join(roleDir, "index.json"), role)
					write(filepath.Join(roleDir, "versions", "1.json"), role)
					bindingPath := filepath.Join(homeDir, "binding.json")
					write(bindingPath, agentsandbox.Binding{HomeDir: homeDir, Role: role.ID, Project: role.Project, DirHash: role.DirHash, Provider: tc.driver})
					self, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, self, "--config", bindingPath, "--", "--project", b)
					cmd.Dir = b
					cmd.Env = append(os.Environ(), "RUBI_AGENT_EXEC_TEST=1")
					var stdout, stderr bytes.Buffer
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr
					err = cmd.Run()
					var exit *exec.ExitError
					if !errors.As(err, &exit) || exit.ExitCode() != 125 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tc.driver) || !strings.Contains(stderr.String(), tc.reason) {
						t.Fatalf("route did not refuse clearly: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatal("refused route executed provider/runtime")
					}
				})
			}
		})
	}
}

func TestGitCredentialFDEntrypoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	} // inherited Unix descriptors are unsupported there; task admission refuses earlier
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = io.WriteString(w, "fake-fd-only-credential"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	quote := "'" + strings.ReplaceAll(helper, "'", "'\\''") + "'"
	cmd := exec.Command("/usr/bin/git", "-c", "credential.helper=", "-c", "credential.helper=!"+quote+" --git-credential-helper", "credential", "fill")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	cmd.ExtraFiles = []*os.File{r}
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	out, err := cmd.Output()
	if err != nil || !strings.Contains(string(out), "password=fake-fd-only-credential") {
		t.Fatal("real Git/helper inherited descriptor exchange failed", err)
	}
	for _, arg := range append(cmd.Args, cmd.Env...) {
		if strings.Contains(arg, "fake-fd-only-credential") {
			t.Fatal("credential reached argv/environment")
		}
	}
}
