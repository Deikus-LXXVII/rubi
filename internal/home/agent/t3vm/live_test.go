package t3vm

// Live tests: they use the real container tool and the runner image (built by EnsureImage when it is missing, which
// takes minutes the first time). They run only with the variables below.
//
//	RUBI_T3VM_LIVE=1              both tests
//	RUBI_T3_CONTRACT=1            also the end-to-end test, with RUBI_T3_BIN (the pinned t3) and RUBI_T3_NODE
//
// No account is used: the model is the scripted Messages API stand-in (runner/test/support/mock.mjs) and the
// sign-in is a made-up credential file.

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
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

var (
	liveImageOnce sync.Once
	liveImageErr  error
)

// liveHost skips unless the live tests are on, and makes sure the container tool and the image are there.
func liveHost(t *testing.T) *agent.AppleContainer {
	t.Helper()
	if os.Getenv("RUBI_T3VM_LIVE") != "1" {
		t.Skip("set RUBI_T3VM_LIVE=1 to run the live tests (they need the container tool and the runner image)")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Fatal("RUBI_T3VM_LIVE=1 but the container tool isn't installed")
	}
	ac := &agent.AppleContainer{EnvDir: shortTemp(t)}
	liveImageOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		liveImageErr = ac.EnsureImage(ctx)
	})
	if liveImageErr != nil {
		t.Fatalf("the runner image: %v", liveImageErr)
	}
	return ac
}

// shortTemp is a real (symlink-free) temporary directory with a short name (unix socket paths are limited).
func shortTemp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "t3v")
	if err != nil {
		t.Fatal(err)
	}
	d, err = filepath.EvalSymlinks(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// ---- the shared sign-in folder (decision 4) ----

func fakeCredential(expiresAt time.Time) []byte {
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "sk-ant-oat01-fake-not-a-token", "refreshToken": "sk-ant-ort01-fake-not-a-token",
		"expiresAt": expiresAt.UnixMilli(), "scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max"}})
	return b
}

const layoutChanged = "the shared sign-in layout changed (docs/plans/t3-integration.md decision 4 and section 11): " +
	"CLAUDE_SECURESTORAGE_CONFIG_DIR may no longer hold the credential and the refresh lock for every role. " +
	"Do not ship this image or pin until the sign-in design is revisited."

// claudeIn runs the image's claude with the config dir and the sign-in dir mounted at their host paths.
func claudeIn(t *testing.T, ac *agent.AppleContainer, cfg, signIn string, withSecureStorage bool, args ...string) string {
	t.Helper()
	argv := []string{"container", "run", "--rm", "--network", "none", "--user", "10001",
		"-v", cfg + ":" + cfg, "-v", signIn + ":" + signIn, "-e", "HOME=/tmp", "-e", "CLAUDE_CONFIG_DIR=" + cfg}
	if withSecureStorage {
		argv = append(argv, "-e", "CLAUDE_SECURESTORAGE_CONFIG_DIR="+signIn)
	}
	argv = append(argv, "--entrypoint", DefaultClaude, ac.ImageTag())
	argv = append(argv, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	return string(out)
}

func TestLiveSharedSignInLayout(t *testing.T) {
	ac := liveHost(t)
	base := shortTemp(t)
	cfg, signIn := filepath.Join(base, "cfg"), filepath.Join(base, "signin")
	for _, d := range []string{cfg, signIn} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cred := filepath.Join(signIn, ".credentials.json")
	if err := os.WriteFile(cred, fakeCredential(time.Now().Add(time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	loggedIn := func(out string) bool {
		var s struct {
			LoggedIn bool `json:"loggedIn"`
		}
		i := strings.Index(out, "{")
		return i >= 0 && json.Unmarshal([]byte(out[i:]), &s) == nil && s.LoggedIn
	}
	if out := claudeIn(t, ac, cfg, signIn, true, "auth", "status"); !loggedIn(out) {
		t.Fatalf("%s\nwith CLAUDE_SECURESTORAGE_CONFIG_DIR set, `auth status` doesn't see the credential:\n%s", layoutChanged, out)
	}
	if out := claudeIn(t, ac, cfg, signIn, false, "auth", "status"); loggedIn(out) {
		t.Fatalf("%s\nwithout the variable, the credential in the sign-in folder is read anyway (it was meant to be per config dir):\n%s", layoutChanged, out)
	}

	// An expired credential and a refresh lock held in the sign-in folder: claude must wait on that lock, so two VMs
	// sharing the folder can't both rotate the token.
	if err := os.WriteFile(cred, fakeCredential(time.Now().Add(-time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(signIn, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out := claudeIn(t, ac, cfg, signIn, true, "-p", "hello")
	if !strings.Contains(out, "another Claude Code process is refreshing it") {
		t.Fatalf("%s\nwith a refresh lock held in the sign-in folder, claude did not refuse to refresh:\n%s", layoutChanged, out)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("%s\nthe refusal took %s (expected seconds)", layoutChanged, d)
	}
	// and the lock is not looked for in the config dir
	if err := os.Remove(filepath.Join(signIn, ".oauth_refresh.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg, ".oauth_refresh.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := claudeIn(t, ac, cfg, signIn, true, "-p", "hello"); strings.Contains(out, "another Claude Code process is refreshing it") {
		t.Fatalf("%s\na lock in the config dir stopped the refresh: the lock is no longer kept in the sign-in folder:\n%s", layoutChanged, out)
	}
}

// ---- the whole path ----

type liveDecider struct {
	mu      sync.Mutex
	verdict string
	reason  string
	asks    []agent.Item
}

func (d *liveDecider) set(verdict, reason string) {
	d.mu.Lock()
	d.verdict, d.reason = verdict, reason
	d.mu.Unlock()
}

func (d *liveDecider) run(ctx context.Context, m *agent.Manager) {
	for ctx.Err() == nil {
		items, err := m.Next(ctx, 200*time.Millisecond)
		if err != nil {
			return
		}
		for _, it := range items {
			d.mu.Lock()
			v, r := d.verdict, d.reason
			if it.Type == "perm.ask" {
				d.asks = append(d.asks, it)
			}
			d.mu.Unlock()
			if it.Type != "perm.ask" {
				v = "deny" // no network, no extra approvals in this test
			}
			_ = m.Decide(it.AskID, v, r, nil)
		}
	}
}

func (d *liveDecider) count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.asks) }

func containersLike(prefix string) []string {
	out, err := exec.Command("container", "list", "--all", "--quiet").CombinedOutput()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Fields(string(out)) {
		if strings.HasPrefix(l, prefix) {
			names = append(names, l)
		}
	}
	return names
}

func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func lastAssistantText(items []t3.Item) string {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == "assistant_message" {
			return items[i].Text
		}
	}
	return ""
}

func TestLiveT3ThreadInARoleVM(t *testing.T) {
	ac := liveHost(t)
	if os.Getenv("RUBI_T3_CONTRACT") != "1" {
		t.Skip("set RUBI_T3_CONTRACT=1 (with RUBI_T3_BIN and RUBI_T3_NODE) for the end-to-end test")
	}
	bin, node := os.Getenv("RUBI_T3_BIN"), os.Getenv("RUBI_T3_NODE")
	if bin == "" || node == "" {
		t.Skip("RUBI_T3_BIN and RUBI_T3_NODE are needed")
	}
	base := shortTemp(t)
	homeDir, project, t3Base := filepath.Join(base, "h"), filepath.Join(base, "p", "my-project"), filepath.Join(base, "t3")
	for _, d := range []string{homeDir, project} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ac.EnvDir = homeDir
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	// Rubi Home's side: the manager with a role pinned to the project, a decider standing in for the person, and the
	// session server with the scripted API behind the VM's 127.0.0.1:3129.
	three := 6
	m := agent.New(agent.Options{Dir: homeDir, Projects: func() []string { return []string{project} },
		GitHosts: func() []string { return nil }, NewVM: func(agent.Project) agent.VM { return &agent.Fake{} }, Wait: 90 * time.Second,
		Usage: func() agent.UsageConfig { return agent.UsageConfig{MaxSessionsPerProfile: 4, MaxSessions: &three} }})
	defer m.Close()
	raw, _ := json.Marshal(map[string]any{"base_version": 0, "by": "test",
		"spec": map[string]any{"project": "my-project", "name": "e2e", "description": "end to end"}})
	res, err := m.Handle(ctx, "claude-agent.role.put", raw)
	if err != nil {
		t.Fatal(err)
	}
	roleID := res.(*roles.PutResult).ID
	roleClaude := filepath.Join(homeDir, "agent-roles", roleID, "claude")
	dec := &liveDecider{}
	dctx, stopDecider := context.WithCancel(ctx)
	defer stopDecider()
	go dec.run(dctx, m)

	// The scripted API, reachable from the VM through the session server.
	mockURL, mockLog := startLiveMock(t, node, base)
	mu, _ := url.Parse(mockURL)

	// T3 itself, on a port that is also the VM's T3 port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	srvT3 := &t3.Server{Node: node, Bin: bin, BaseDir: t3Base, Port: port}
	t.Cleanup(srvT3.Stop)
	if _, err := srvT3.Start(ctx); err != nil {
		t.Fatal(err)
	}

	sessions := NewServer(Options{Dir: homeDir, Backend: m, Host: ac, T3Port: port, Attachments: filepath.Join(t3Base, "userdata", "attachments"),
		testEnv: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:3129", "ANTHROPIC_API_KEY": "sk-ant-mock-not-a-key-0000000000"},
		testAPI: func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "tcp", mu.Host) },
		Logf:    t.Logf})
	sctx, stopSessions := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- sessions.Serve(sctx, SocketPath(homeDir)) }()
	defer func() { stopSessions(); <-served }()

	// The role's instance, with a shim that runs this checkout's rubi-home.
	rubiHome := filepath.Join(base, "rubi-home")
	build := exec.Command("go", "build", "-o", rubiHome, "./cmd/rubi-home")
	build.Dir = repoRootDir(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rubi-home: %v\n%s", err, out)
	}
	syn, err := SyncInstances(InstanceOptions{T3Base: t3Base, Dir: homeDir, RubiHome: rubiHome},
		[]RoleInstance{{ID: roleID, Name: "e2e", ClaudeDir: roleClaude}})
	if err != nil || len(syn.Added) != 1 {
		t.Fatalf("instances: %+v %v", syn, err)
	}
	instance := InstancePrefix + roleID

	access := "full-access"
	code, err := srvT3.PairingCode(ctx, t3.PairingScopes(access), 10*time.Minute, "rubi t3vm live test")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := t3.Pair(ctx, http.DefaultClient, srvT3.Base(), code, access, "Rubi t3vm live test")
	if err != nil {
		t.Fatal(err)
	}
	c := t3.New(srvT3.Base(), tok, t3.FullAccess)
	t.Cleanup(func() { _ = c.Close() })
	p, err := c.CreateProject(ctx, project, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	wait := func(threadID string) t3.WaitResult {
		t.Helper()
		for i := 0; i < 40; i++ {
			w, err := c.Wait(ctx, threadID, 15*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if w.Done() {
				return w
			}
		}
		t.Fatal("the thread did not end")
		return t3.WaitResult{}
	}

	// 1. A Bash call that the decider denies: the thread completes, the file is not made, the model saw the reason.
	dec.set("deny", "Rubi test says no")
	in := t3.LaunchIn{ProjectID: p.ID, Title: "deny", Instance: instance, Model: "claude-sonnet-4-6", Mode: t3.FullAccess,
		Message: "MOCKSCRIPT:bashrun go", RequestID: "launch-1"}
	var run t3.Run
	for i := 0; ; i++ { // T3 watches its settings file: the instance may need a moment
		if run, err = c.Launch(ctx, in); err == nil || i >= 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal(err)
	}
	if w := wait(run.ThreadID); w.Status != "completed" {
		t.Fatalf("the thread ended %s\n%s", w.Status, mockLog())
	}
	if _, err := os.Stat(filepath.Join(project, "ran.txt")); err == nil {
		t.Fatal("the denied Bash call ran")
	}
	if dec.count() < 1 {
		t.Fatal("the decider was never asked: the hook did not reach Home")
	}
	dec.mu.Lock()
	ask := dec.asks[0]
	dec.mu.Unlock()
	if ask.Tool != "Bash" || ask.Via != "hook" || ask.Role != roleID || ask.Project != "my-project" || ask.Cwd != project {
		t.Fatalf("the ask: %+v", ask)
	}
	if !strings.Contains(mockLog(), "Rubi test says no") {
		t.Fatalf("the model did not see the deny reason:\n%s", mockLog())
	}
	// the VM is there while the provider session is
	if n := containersLike("rubi-t3-"); len(n) == 0 {
		t.Error("no VM for the live thread")
	}
	if sessions.Sessions() < 1 {
		t.Error("Home counts no session")
	}

	// 2. The same call, allowed: the file is made, inside the VM, in the project.
	dec.set("allow", "")
	if _, err := c.Send(ctx, run.ThreadID, "MOCKSCRIPT:bashrun again", "send-allow"); err != nil {
		t.Fatal(err)
	}
	if w := wait(run.ThreadID); w.Status != "completed" {
		t.Fatalf("the second turn ended %s\n%s", w.Status, mockLog())
	}
	if b, err := os.ReadFile(filepath.Join(project, "ran.txt")); err != nil || strings.TrimSpace(string(b)) != "RAN" {
		t.Fatalf("the allowed Bash call did not write ran.txt: %q %v\n%s", b, err, mockLog())
	}

	// 3. History and a fork: the session files are on the host, in the role's config dir, and the fork sees them.
	_, items, err := c.Read(ctx, run.ThreadID, 0)
	if err != nil || lastAssistantText(items) != "bash step done" {
		t.Fatalf("history: %q %v", lastAssistantText(items), err)
	}
	files, _ := filepath.Glob(filepath.Join(roleClaude, "projects", "*", "*.jsonl"))
	if len(files) == 0 {
		t.Errorf("no session file in the role's config dir %s", roleClaude)
	}
	fork, err := c.Fork(ctx, run.ThreadID, "fork")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, fork, "and in the fork?", "send-fork"); err != nil {
		t.Fatal(err)
	}
	if w := wait(fork); w.Status != "completed" {
		t.Fatalf("the fork ended %s\n%s", w.Status, mockLog())
	}
	_, fitems, err := c.Read(ctx, fork, 0)
	if err != nil || !strings.Contains(lastAssistantText(fitems), "done (bashrun)") {
		t.Fatalf("the fork's reply %q (%v): the history did not carry over", lastAssistantText(fitems), err)
	}

	// 4. When T3's provider sessions end, the VMs go.
	srvT3.Stop()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && (sessions.Sessions() > 0 || len(containersLike("rubi-t3-")) > 0) {
		time.Sleep(500 * time.Millisecond)
	}
	if n := sessions.Sessions(); n != 0 {
		t.Errorf("Home still counts %d session(s) after T3 stopped", n)
	}
	if n := containersLike("rubi-t3-"); len(n) != 0 {
		t.Errorf("VMs left after T3 stopped: %v", n)
	}
}

// startLiveMock runs the scripted API stand-in and returns its URL and a function that reads its request log.
func startLiveMock(t *testing.T, node, dir string) (string, func() string) {
	t.Helper()
	urlFile, logFile := filepath.Join(dir, "mock.url"), filepath.Join(dir, "mock.log")
	cmd := exec.Command(node, filepath.Join(repoRootDir(t), "t3", "contract", "mockserve.mjs"), urlFile, logFile)
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
