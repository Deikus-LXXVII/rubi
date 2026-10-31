package home

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// TestLiveSetupStatus probes this computer through a fresh Rubi Home directory and prints the setup page's
// status. Skipped unless RUBI_AGENT_LIVE=1; RUBI_AGENT_LIVE_PROJECT (optional) is added as an agent project.
// Status alone only reads; RUBI_AGENT_LIVE_RUN may run the service or image step (no privilege needed).
func TestLiveSetupStatus(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" {
		t.Skip("set RUBI_AGENT_LIVE=1 to probe this computer")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("RUBI_AGENT_LIVE_PROJECT"); p != "" {
		if err := h.Update(func(c *Config) { c.AgentProjects = []string{p} }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// RUBI_AGENT_LIVE_RUN names one unprivileged step to run first (service, image).
	if step := os.Getenv("RUBI_AGENT_LIVE_RUN"); step == "service" || step == "image" {
		if err := h.Setup.Run(ctx, step, "", "", ""); err != nil {
			t.Fatal(err)
		}
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(2 * time.Second)
		}
	}
	// RUBI_AGENT_LIVE_RUN=login-url starts the shared sign-in, waits for its link and cancels: no code is sent.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "login-url" {
		if err := h.Setup.Run(ctx, "login", "", "claudeai", ""); err != nil {
			t.Fatal(err)
		}
		url := ""
		for i := 0; i < 90 && url == "" && h.Setup.Busy(); i++ {
			time.Sleep(2 * time.Second)
			if l := h.Setup.Status(ctx).Login; l != nil {
				url = l.URL
			}
		}
		h.Setup.Cancel()
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(time.Second)
		}
		if !strings.HasPrefix(url, "https://claude.com/") {
			t.Errorf("no sign-in link: %q", url)
		} else {
			t.Logf("sign-in link host ok (%d characters)", len(url))
		}
	}
	// RUBI_AGENT_LIVE_RUN=share: two project VMs run at once, both mount the sign-in directory read-only, and a
	// sign-in-style container with it read-write writes a dummy file (never a token) that both see.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "share" {
		liveShare(ctx, t, h)
	}
	// RUBI_AGENT_LIVE_RUN=gateway: a project VM is started through the manager with a FAKE token file and a local
	// TLS stand-in for api.anthropic.com. The fake token must be nowhere in the VM, and a request through the
	// gateway must reach the stand-in with the token added.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "gateway" {
		liveGateway(ctx, t, h)
	}
	b, _ := json.MarshalIndent(h.Setup.Status(ctx), "", "  ")
	t.Logf("status:\n%s", b)
}

func liveShare(ctx context.Context, t *testing.T, h *Helper) {
	t.Helper()
	cfg := h.Fresh()
	a := &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, Network: cfg.AgentNetwork, EnvDir: h.Dir, AuthDir: cfg.AgentAuthDir}
	if err := a.EnsureImage(ctx); err != nil {
		t.Fatalf("image: %v", err)
	}
	if err := a.EnsureAuthDir(); err != nil {
		t.Fatal(err)
	}
	authDir := cfg.AgentAuthDir
	if authDir == "" {
		authDir = agent.AuthDirOf(h.Dir)
	}
	const dummy = "dummy-share-check" // a made-up name and content: this test never touches a real token
	defer os.Remove(filepath.Join(authDir, "token", dummy))

	projects := []agent.Project{{ID: "share-a", Dir: t.TempDir()}, {ID: "share-b", Dir: t.TempDir()}}
	for i := range projects {
		dir, err := filepath.EvalSymlinks(projects[i].Dir)
		if err != nil {
			t.Fatal(err)
		}
		projects[i].Dir = dir
		args := a.RunArgs(projects[i])
		var name string
		for j, x := range args {
			if x == "--name" {
				name = args[j+1]
			}
		}
		_ = exec.Command(args[0], "delete", "--force", name).Run()
		if out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("starting %s: %v: %s", projects[i].ID, err, out)
		}
		defer exec.Command(args[0], "delete", "--force", name).Run()
	}

	// The sign-in-style container: the directory read-write.
	sess, err := a.StartAuth(ctx)
	if err != nil {
		t.Fatalf("sign-in container: %v", err)
	}
	defer sess.Close()
	c, err := sess.Exec(ctx, []string{"sh", "-c", "echo not-a-token > /rubi-auth/token/" + dummy + " && echo wrote"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWrite()
	out, _ := io.ReadAll(c)
	_ = c.Close()
	if !strings.Contains(string(out), "wrote") {
		t.Fatalf("the sign-in container couldn't write: %q", out)
	}

	for _, p := range projects {
		argv := a.ExecArgs(p, "", []string{"sh", "-c",
			"cat /rubi-auth/token/" + dummy + "; touch /rubi-auth/token/other 2>&1; echo touch-rc=$?; ln -s x /rubi-auth/token/link 2>&1; echo ln-rc=$?"})
		got, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", p.ID, err, got)
		}
		res := string(got)
		t.Logf("%s sees: %s", p.ID, strings.ReplaceAll(strings.TrimSpace(res), "\n", " | "))
		if !strings.Contains(res, "not-a-token") {
			t.Errorf("%s doesn't see the file the other container wrote", p.ID)
		}
		if strings.Contains(res, "touch-rc=0") || strings.Contains(res, "ln-rc=0") {
			t.Errorf("%s could write to the read-only directory", p.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(authDir, "token", "other")); err == nil {
		t.Error("a read-only VM created a file")
	}
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
	mgr := agent.New(agent.Options{
		Dir: h.Dir, Projects: func() []string { return []string{proj} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, Network: cfg.AgentNetwork, EnvDir: h.Dir}
		},
		GatewayUpstream: upstream.URL, GatewayClient: upstream.Client(),
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
