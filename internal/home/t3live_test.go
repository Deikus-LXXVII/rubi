//go:build t3vmtest

package home

// The live check of the whole path through Rubi Home's op handler: t3-agent.start runs a thread of a role in a VM, the
// hook's tool ask arrives through t3-agent.next, t3-agent.decide allows it, and the finished turn arrives as an item.
// It needs the container tool, the runner image and the pinned T3, and runs only with
//
//	go test -tags t3vmtest ./internal/home -run TestLiveT3AgentThroughHome
//	RUBI_T3VM_LIVE=1 RUBI_T3_CONTRACT=1 RUBI_T3_BIN=<the pinned t3 bin> RUBI_T3_NODE=<node>
//
// No account is used: the model is the scripted Messages API stand-in (t3/contract/mockserve.mjs).

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

func liveRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found")
	}
}

func liveMock(t *testing.T, node, dir string) (string, func() string) {
	t.Helper()
	urlFile, logFile := filepath.Join(dir, "mock.url"), filepath.Join(dir, "mock.log")
	cmd := exec.Command(node, filepath.Join(liveRepoRoot(t), "t3", "contract", "mockserve.mjs"), urlFile, logFile)
	cmd.Env = []string{"PATH=" + filepath.Dir(node) + ":/usr/bin:/bin"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(urlFile); err == nil && len(b) > 0 {
			return string(b), func() string { b, _ := os.ReadFile(logFile); return string(bytes.TrimSpace(b)) }
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the mock did not start")
	return "", nil
}

func TestLiveT3AgentThroughHome(t *testing.T) {
	if os.Getenv("RUBI_T3VM_LIVE") != "1" || os.Getenv("RUBI_T3_CONTRACT") != "1" {
		t.Skip("set RUBI_T3VM_LIVE=1 and RUBI_T3_CONTRACT=1 (with RUBI_T3_BIN and RUBI_T3_NODE) for the live path")
	}
	bin, node := os.Getenv("RUBI_T3_BIN"), os.Getenv("RUBI_T3_NODE")
	if bin == "" || node == "" {
		t.Skip("RUBI_T3_BIN and RUBI_T3_NODE are needed")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Fatal("the container tool isn't installed")
	}
	base, err := os.MkdirTemp("", "t3h")
	if err != nil {
		t.Fatal(err)
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	homeDir, project, t3Base := filepath.Join(base, "h"), filepath.Join(base, "p", "my-project"), filepath.Join(base, "t3")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	h, err := Open(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	ac := &agent.AppleContainer{EnvDir: homeDir}
	if err := ac.EnsureImage(ctx); err != nil {
		t.Fatalf("the runner image: %v", err)
	}
	t3port := freeTestPort(t)
	if err := h.Update(func(c *Config) {
		c.AgentProjects = []string{project}
		c.Paired = []Pairing{{TokenHash: tokenHash("tok")}}
	}); err != nil {
		t.Fatal(err)
	}
	call := func(op string, args any) (map[string]any, error) { return handle(t, h, "tok", op, args) }

	put, err := call("t3-agent.role.put", map[string]any{"base_version": 0, "by": "test",
		"spec": map[string]any{"project": "my-project", "name": "e2e", "description": "end to end"}})
	if err != nil {
		t.Fatal(err)
	}
	roleID := put["id"].(string)

	// T3 itself, paired the way Home pairs it (a code minted on this computer, full access).
	srv := &t3.Server{Node: node, Bin: bin, BaseDir: t3Base, Port: t3port}
	t.Cleanup(srv.Stop)
	if _, err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	code, err := srv.PairingCode(ctx, t3.PairingScopes("full-access"), 10*time.Minute, "rubi home live test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := t3.Pair(ctx, http.DefaultClient, srv.Base(), code, "full-access", "Rubi Home live test")
	if err != nil {
		t.Fatal(err)
	}
	rc, stop := context.WithCancel(ctx)
	defer stop()
	cl := t3.New(srv.Base(), tok, t3.FullAccess)
	t.Cleanup(func() { _ = cl.Close() })
	h.T3.mu.Lock()
	h.T3.srv, h.T3.cl, h.T3.runCtx, h.T3.runStop = srv, cl, rc, stop
	h.T3.mu.Unlock()
	h.T3Pid = srv.Pid

	// The role's instance runs this checkout's rubi-home; the session server has the scripted API behind the VM.
	rubiHome := filepath.Join(base, "rubi-home")
	build := exec.Command("go", "build", "-o", rubiHome, "./cmd/rubi-home")
	build.Dir = liveRepoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rubi-home: %v\n%s", err, out)
	}
	if res, err := t3vm.SyncInstances(t3vm.InstanceOptions{T3Base: t3Base, Dir: homeDir, RubiHome: rubiHome},
		[]t3vm.RoleInstance{{ID: roleID, Name: "e2e", ClaudeDir: filepath.Join(homeDir, "agent-roles", roleID, "claude")}}); err != nil || len(res.Added) != 1 {
		t.Fatalf("instances: %+v %v", res, err)
	}
	mockURL, mockLog := liveMock(t, node, base)
	mu, _ := url.Parse(mockURL)
	opt := t3vm.Options{Dir: homeDir, Backend: h.Agent, Host: ac, T3Port: t3port, T3Pid: srv.Pid, Attachments: filepath.Join(t3Base, "userdata", "attachments"), Logf: t.Logf}
	opt.SetTestAPI(map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:3129", "ANTHROPIC_API_KEY": "sk-ant-mock-not-a-key-0000000000"},
		func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", mu.Host) })
	sessions := t3vm.NewServer(opt)
	sctx, stopSessions := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- sessions.Serve(sctx, AgentVMSocketPath(homeDir)) }()
	defer func() { stopSessions(); <-served }()

	// Start a session through the op, in the mode that makes claude run commands itself.
	var started map[string]any
	for i := 0; ; i++ { // T3 watches its settings file: the instance may need a moment
		if started, err = call("t3-agent.start", map[string]any{"project": "my-project", "role": roleID, "prompt": "MOCKSCRIPT:bashrun go",
			"mode": "bypassPermissions", "from": "Tester", "bot": "tester"}); err == nil || i >= 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	id, _ := started["id"].(string)
	if id == "" {
		t.Fatalf("start: %v", started)
	}

	// next yields the hook's ask; decide allows it; the finished turn arrives.
	var ask map[string]any
	var result map[string]any
	allowed := false
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) && result == nil {
		res, err := call("t3-agent.next", map[string]any{"wait_seconds": 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range res["items"].([]any) {
			it := raw.(map[string]any)
			switch it["type"] {
			case "perm.ask":
				if ask == nil {
					ask = it
				}
				if _, err := call("t3-agent.decide", map[string]any{"ask_id": it["ask_id"], "behavior": "allow"}); err != nil {
					t.Fatal(err)
				}
				allowed = true
			case "event":
				ev, _ := it["event"].(map[string]any)
				if ev["kind"] == "result" && allowed {
					result = ev
				}
			}
		}
	}
	if ask == nil {
		t.Fatalf("no tool ask arrived through next\n%s", mockLog())
	}
	if ask["tool"] != "Bash" || ask["via"] != "hook" || ask["role"] != roleID || ask["project"] != "my-project" || ask["cwd"] != project || ask["session_id"] != id {
		t.Fatalf("the ask: %v (thread %s)", ask, id)
	}
	if result == nil {
		t.Fatalf("no finished-turn item arrived\n%s", mockLog())
	}
	if result["session_id"] != id || result["to"] != "Tester" || result["truncated"] != false || strings.TrimSpace(result["text"].(string)) == "" || result["is_error"] != false {
		t.Fatalf("the result: %v\n%s", result, mockLog())
	}
	if b, err := os.ReadFile(filepath.Join(project, "ran.txt")); err != nil || strings.TrimSpace(string(b)) != "RAN" {
		t.Errorf("the allowed Bash call did not run in the VM: %q %v", b, err)
	}
	rows, err := call("t3-agent.sessions", map[string]any{"project": "my-project", "role": roleID})
	if err != nil {
		t.Fatal(err)
	}
	if s := rows["sessions"].([]any); len(s) != 1 || s[0].(map[string]any)["busy"] != false || s[0].(map[string]any)["dir"] != project {
		t.Errorf("sessions: %v", rows)
	}
	b, _ := json.Marshal(result)
	t.Logf("finished-turn item: %s", b)
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	p, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
