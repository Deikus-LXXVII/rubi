package agent

// Live checks against a real Apple container VM (docs/design/agent-runner.md, "Live checks"). They are
// skipped unless RUBI_AGENT_LIVE=1; they need the container tool, its system service, and the runner
// image (built from runner/ when RUBI_AGENT_RUNNER_DIR is set). RUBI_AGENT_LIVE_PROJECT is the project
// directory to mount (it keeps the VM's Claude login in .rubi/claude between runs); its base name is the
// project id. Nothing here prints a token: the run and proxy tokens stay in memory and in the 0600
// environment file of the exec.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveEnv struct {
	t    *testing.T
	m    *Manager
	ctx  context.Context
	id   string
	vm   *AppleContainer
	mu   sync.Mutex
	asks []string // hosts net.ask was asked about, in order
}

func newLive(t *testing.T) *liveEnv {
	t.Helper()
	if os.Getenv("RUBI_AGENT_LIVE") != "1" {
		t.Skip("set RUBI_AGENT_LIVE=1 to run against a real container VM")
	}
	proj := os.Getenv("RUBI_AGENT_LIVE_PROJECT")
	if proj == "" || !filepath.IsAbs(proj) {
		t.Fatal("RUBI_AGENT_LIVE_PROJECT must be an absolute project directory")
	}
	proj, err := filepath.EvalSymlinks(proj)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	l := &liveEnv{t: t, ctx: ctx, id: filepath.Base(proj)}
	l.vm = &AppleContainer{RunnerDir: os.Getenv("RUBI_AGENT_RUNNER_DIR"), Network: os.Getenv("RUBI_AGENT_NETWORK"), EnvDir: t.TempDir()}
	l.m = New(Options{Dir: t.TempDir(), Projects: func() []string { return []string{proj} },
		GitHosts: func() []string { return nil }, NewVM: func(Project) VM { return l.vm }, Wait: 2 * time.Minute})
	t.Cleanup(func() {
		l.m.Close()
		_ = l.vm.Stop(context.Background())
	})
	return l
}

// answer plays the plugin: it answers every ask with decide and hands events to onEvent until it returns
// true or the context ends. Hosts asked about are allowed only for Anthropic's and Claude's domains.
func (l *liveEnv) answer(ctx context.Context, decide func(it Item) (string, string), onEvent func(it Item) bool) {
	for ctx.Err() == nil {
		items, err := l.m.Next(ctx, 20*time.Second)
		if err != nil {
			return
		}
		for _, it := range items {
			switch it.Type {
			case "net.ask":
				l.mu.Lock()
				l.asks = append(l.asks, it.Host)
				l.mu.Unlock()
				b := "deny"
				if anthropicHost(it.Host) {
					b = "allow"
				}
				l.t.Logf("net.ask %s -> %s", it.Host, b)
				_ = l.m.Decide(it.AskID, b, "live check", nil)
			case "perm.ask", "cred.ask":
				b, msg := decide(it)
				l.t.Logf("%s %s %s -> %s", it.Type, it.Tool, liveClip(string(it.Input), 200), b)
				_ = l.m.Decide(it.AskID, b, msg, nil)
			case "event":
				if onEvent(it) {
					return
				}
			}
		}
	}
}

func liveClip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func anthropicHost(h string) bool {
	for _, d := range []string{"anthropic.com", "claude.ai", "claude.com"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

func (l *liveEnv) start() {
	l.t.Helper()
	b, _ := json.Marshal(map[string]any{"project": l.id})
	if _, err := l.m.Handle(l.ctx, "claude-agent.sessions", b); err != nil {
		l.t.Fatalf("starting the VM and the runner: %v", err)
	}
}

// probe runs a node script in the VM as the runner's user and returns what it printed.
func (l *liveEnv) probe(script string, env map[string]string) string {
	l.t.Helper()
	c, err := l.vm.Exec(l.ctx, []string{"node", "-e", script}, env)
	if err != nil {
		l.t.Fatal(err)
	}
	defer c.Close()
	_ = c.CloseWrite()
	out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
	return string(out)
}

func (l *liveEnv) proxyURL() string {
	l.m.mu.Lock()
	defer l.m.mu.Unlock()
	px := l.m.projs[l.id].proxy
	if px == nil {
		l.t.Fatal("no proxy")
	}
	host, port, _ := net.SplitHostPort(px.Addr())
	return "http://rubi:" + px.cfg.AuthToken + "@" + net.JoinHostPort(host, port)
}

const egressProbe = `
const net = require("net"), dns = require("dns"), fs = require("fs");
const out = {};
try { fs.mkdirSync("/work/.rubi", {recursive: true}); fs.writeFileSync("/work/.rubi/live-probe", "ok"); out.write = "ok"; } catch (e) { out.write = e.code; }
out.uid = process.getuid();
const tcp = (host, port) => new Promise(r => { const s = net.connect({host, port, timeout: 4000});
  s.on("connect", () => { s.destroy(); r("connected"); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
const connect = (host) => new Promise(r => { const u = new URL(process.env.HTTPS_PROXY);
  const s = net.connect({host: u.hostname, port: +u.port, timeout: 15000}, () => s.write("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host +
    ":443\r\nProxy-Authorization: Basic " + Buffer.from(decodeURIComponent(u.username) + ":" + decodeURIComponent(u.password)).toString("base64") + "\r\n\r\n"));
  s.once("data", d => { s.destroy(); r(d.toString().split("\r\n")[0]); }); s.on("timeout", () => { s.destroy(); r("timeout"); }); s.on("error", e => r(e.code)); });
(async () => {
  out.dns = await new Promise(r => dns.lookup("registry.npmjs.org", (e, a) => r(e ? e.code : a)));
  out.direct_ip = await tcp("1.1.1.1", 443);
  out.direct_default_gw = await tcp("192.168.64.1", 53);
  out.proxy_allowed = await connect("registry.npmjs.org");
  out.proxy_not_listed = await connect("example.org");
  console.log(JSON.stringify(out));
})();`

// The VM writes the project, reaches nothing directly, and reaches allowed hosts only through the proxy.
func TestLiveContainerIsolation(t *testing.T) {
	l := newLive(t)
	l.start()
	go l.answer(l.ctx, func(Item) (string, string) { return "deny", "live check" }, func(Item) bool { return false })
	out := l.probe(egressProbe, map[string]string{"HTTPS_PROXY": l.proxyURL()})
	t.Logf("probe: %s", strings.TrimSpace(out))
	var r map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil {
		t.Fatalf("probe output: %q", out)
	}
	if r["write"] != "ok" || r["uid"] != float64(10001) {
		t.Errorf("the runner's user can't write /work/.rubi: %v", r)
	}
	if r["dns"] == nil || strings.Contains(fmt.Sprint(r["dns"]), ".") {
		t.Errorf("the VM resolves names itself: %v", r["dns"])
	}
	for _, k := range []string{"direct_ip", "direct_default_gw"} {
		if r[k] == "connected" {
			t.Errorf("direct egress works (%s)", k)
		}
	}
	if !strings.Contains(fmt.Sprint(r["proxy_allowed"]), " 200") {
		t.Errorf("an allowed host isn't reachable through the proxy: %v", r["proxy_allowed"])
	}
	if strings.Contains(fmt.Sprint(r["proxy_not_listed"]), " 200") {
		t.Errorf("a host not on the list was let through: %v", r["proxy_not_listed"])
	}
}

// TestLiveLogin keeps the VM and its proxy up and runs `claude auth login` in it on this process's
// terminal, so the owner can sign in through the browser; hosts the login asks for are logged. Run the
// compiled test binary directly in a terminal (go test does not hand it the terminal):
//
//	go test -c -o /tmp/agent-live ./internal/home/agent
//	RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_LOGIN=1 RUBI_AGENT_LIVE_PROJECT=$HOME/my-project /tmp/agent-live -test.run TestLiveLogin -test.v
func TestLiveLogin(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE_LOGIN") != "1" {
		t.Skip("set RUBI_AGENT_LIVE_LOGIN=1 (interactive)")
	}
	l := newLive(t)
	l.start()
	go l.answer(l.ctx, func(Item) (string, string) { return "deny", "live check" }, func(Item) bool { return false })
	envFile := filepath.Join(t.TempDir(), "env")
	body, _ := EnvFile(map[string]string{"HTTPS_PROXY": l.proxyURL()})
	if err := os.WriteFile(envFile, body, 0o600); err != nil {
		t.Fatal(err)
	}
	name := containerName(l.vm.project())
	bin := "/opt/rubi-runner/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64/claude"
	cmd := exec.CommandContext(l.ctx, l.vm.bin(), "exec", "-it", "--user", runnerUID, "--workdir", "/work",
		"--env-file", envFile, name, bin, "auth", "login", "--claudeai")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Errorf("login: %v", err)
	}
	st := exec.CommandContext(l.ctx, l.vm.bin(), "exec", "--user", runnerUID, "--env-file", envFile, name, bin, "auth", "status")
	out, _ := st.CombinedOutput()
	t.Logf("auth status: %s", statusSummary(out))
	l.mu.Lock()
	t.Logf("hosts asked during login: %v", l.asks)
	l.mu.Unlock()
}

// statusSummary keeps only the non-secret fields of `claude auth status` (it prints JSON).
func statusSummary(out []byte) string {
	var v map[string]any
	if json.Unmarshal(out, &v) != nil {
		return fmt.Sprintf("(not JSON, %d bytes)", len(out))
	}
	keep := map[string]any{}
	for _, k := range []string{"loggedIn", "authMethod", "apiProvider", "subscriptionType"} {
		if x, ok := v[k]; ok {
			keep[k] = x
		}
	}
	b, _ := json.Marshal(keep)
	return string(b)
}

// One real turn on the owner's subscription: the model is asked to run a Bash command, the plugin side
// (this test) denies it through perm.ask, and the turn must end with a result that went through the proxy.
func TestLiveTurnAndDeny(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE_TURN") != "1" {
		t.Skip("set RUBI_AGENT_LIVE_TURN=1 (uses the signed-in subscription)")
	}
	l := newLive(t)
	l.start()
	var denied, result bool
	var resultText string
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.answer(l.ctx, func(it Item) (string, string) {
			if it.Tool == "Bash" {
				denied = true
				return "deny", "the live check denies Bash"
			}
			return "deny", "only Bash is expected"
		}, func(it Item) bool {
			var ev struct {
				Kind    string `json:"kind"`
				State   string `json:"state"`
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			_ = json.Unmarshal(it.Event, &ev)
			if ev.Kind != "status" {
				t.Logf("event %s %s", ev.Kind, liveClip(string(it.Event), 300))
			}
			if ev.Kind == "result" && it.AgentID == "" {
				result, resultText = true, ev.Result
				return true
			}
			return false
		})
	}()
	b, _ := json.Marshal(map[string]any{"project": l.id, "mode": "default", "name": "live check",
		"prompt": "Use the Bash tool to run exactly `echo live-check`. If the tool call is denied, reply with the single word DENIED and nothing else."})
	res, err := l.m.Handle(l.ctx, "claude-agent.start", b)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("start: %v", res)
	select {
	case <-done:
	case <-time.After(10 * time.Minute):
		t.Fatal("no result in 10 minutes")
	}
	if !denied || !result {
		t.Fatalf("denied=%v result=%v", denied, result)
	}
	t.Logf("result: %q", liveClip(resultText, 200))
	l.mu.Lock()
	t.Logf("hosts asked: %v", l.asks)
	l.mu.Unlock()
}
