package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if os.Getenv("RUBI_AGENT_EXEC_TEST") == "1" {
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
