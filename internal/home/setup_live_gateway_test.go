//go:build gatewaytest

package home

// Live check of the auth gateway. It needs the test hook of the agent package, so it is built only with
//
//	RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=gateway go test -tags gatewaytest ./internal/home -run TestLiveGateway -v
//
// A project VM is started through the manager with a FAKE token file and a local TLS stand-in for
// api.anthropic.com. The fake token must be nowhere in the VM, and a request through the gateway must reach the
// stand-in with the token added.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

func TestLiveGateway(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" || os.Getenv("RUBI_AGENT_LIVE_RUN") != "gateway" {
		t.Skip("set RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=gateway")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	liveGateway(ctx, t, h)
}

func liveGateway(ctx context.Context, t *testing.T, h *Helper) {
	t.Helper()
	const mark = "FAKELIVEGATEWAYMARK0123456789"
	fake := "sk-ant-oat01-" + mark + "_abcdefghij-KLMNOP" // made up, never a credential
	authTokenDir := filepath.Join(agent.AuthDirOf(h.Dir), "token")
	if err := os.MkdirAll(authTokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(authTokenDir, "oauth-token")
	if err := os.WriteFile(tokenFile, []byte(fake+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tokenFile)

	var mu sync.Mutex
	var seen []http.Header
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","content":[]}`)
	}))
	defer upstream.Close()

	cfg := h.Fresh()
	proj, err := filepath.EvalSymlinks(filepath.Join(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	proj = filepath.Join(proj, "gateway-check")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	agent.SetTestGatewayUpstream(upstream.URL, upstream.Client())
	defer agent.SetTestGatewayUpstream("", nil)
	mgr := agent.New(agent.Options{
		Dir: h.Dir, Projects: func() []string { return []string{proj} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, EnvDir: h.Dir}
		},
	})
	defer mgr.Close()
	sb, err := mgr.Sandbox(ctx, "gateway-check")
	if err != nil {
		t.Fatalf("starting the project VM: %v", err)
	}
	run := func(script string, argv ...string) string {
		t.Helper()
		c, err := sb.Exec(ctx, append(argv, script), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.CloseWrite()
		out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
		_ = c.Close()
		return string(out)
	}
	// 1. The token is nowhere in the VM: no file, no process environment.
	out := run("grep -rIl --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev "+mark+" / 2>/dev/null; echo grep-done; "+
		"cat /proc/[0-9]*/environ 2>/dev/null | tr '\\0' '\\n' | grep -c "+mark+"; true", "sh", "-c")
	t.Logf("search in the VM: %s", strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
	before, after, _ := strings.Cut(out, "grep-done")
	if strings.TrimSpace(before) != "" {
		t.Errorf("the fake token is in files of the VM: %s", before)
	}
	if n := strings.TrimSpace(after); n != "0" {
		t.Errorf("the fake token is in a process environment of the VM (%s)", n)
	}
	// 2. A request through the gateway reaches the stand-in with the token added, and nothing the client sent.
	probe := `const http = require("http"), u = new URL(process.env.ANTHROPIC_BASE_URL);
const go = (secret) => new Promise((res) => { const r = http.request({host: u.hostname, port: u.port, path: "/v1/messages?beta=true", method: "POST",
  headers: {"authorization": "Bearer " + secret, "x-api-key": "client-key", "cookie": "a=b", "content-type": "application/json", "anthropic-version": "2023-06-01"}},
  (x) => { x.resume(); x.on("end", () => res(x.statusCode)); }); r.on("error", (e) => res(e.code)); r.end("{}"); });
(async () => { console.log("good=" + await go(process.env.CLAUDE_CODE_OAUTH_TOKEN)); console.log("wrong=" + await go("not-the-secret-" + "x".repeat(40))); })();`
	got := run(probe, "node", "-e")
	t.Logf("through the gateway: %s", strings.ReplaceAll(strings.TrimSpace(got), "\n", " "))
	if !strings.Contains(got, "good=200") || !strings.Contains(got, "wrong=401") {
		t.Errorf("gateway answers: %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("%d requests reached the stand-in, want 1", len(seen))
	}
	hd := seen[0]
	if hd.Get("Authorization") != "Bearer "+fake || hd.Get("X-Api-Key") != "" || hd.Get("Cookie") != "" {
		t.Errorf("the stand-in got Authorization=%q x-api-key=%q cookie=%q", redactLive(hd.Get("Authorization")), hd.Get("X-Api-Key"), hd.Get("Cookie"))
	}
}

// redactLive keeps the scheme only, so a failing check never prints a credential.
func redactLive(auth string) string {
	if i := strings.Index(auth, " "); i > 0 {
		return auth[:i] + " <...>"
	}
	return "<...>"
}
