package t3vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// ---- fakes ----

type fakeBackend struct {
	fakeAsker
	mu        sync.Mutex
	role      *agent.T3Role
	roleErr   error
	perRole   int
	total     int
	released  int
	proxies   []*agent.Proxy
	proxyAuth string
}

func (b *fakeBackend) T3Role(id string) (*agent.T3Role, error) {
	if b.roleErr != nil {
		return nil, b.roleErr
	}
	if id != b.role.ID {
		return nil, errors.New("unknown role")
	}
	r := *b.role
	r.Release = func() { b.mu.Lock(); b.released++; b.mu.Unlock() }
	return &r, nil
}

func (b *fakeBackend) T3Proxy(r *agent.T3Role, ln net.Listener) (*agent.Proxy, string, error) {
	tok := strings.Repeat("p", 32)
	px, err := agent.StartProxy(agent.ProxyConfig{Listener: ln, AuthToken: tok, Hosts: []string{agent.AnthropicHost}})
	b.mu.Lock()
	b.proxies = append(b.proxies, px)
	b.mu.Unlock()
	return px, tok, err
}

func (b *fakeBackend) T3Caps() (int, int) { return b.perRole, b.total }

func (b *fakeBackend) releasedCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.released }

type fakeHost struct {
	mu         sync.Mutex
	runs       [][]string
	removed    []string
	pipes      []*agent.PipeSpec
	pipeEnvs   []map[string]string
	pipeStops  int
	ensureErr  error
	runErr     error
	afterRun   func()
	ensureHook func()
	pipeErr    error
	block      chan struct{} // EnsureImage waits for it (or its context)
	giveUps    []func()
	outputs    [][]string
	outputErr  error
	outputGate chan struct{} // RunOutput waits for it
	list       []string      // what ListContainers says
	listErr    error
	badMounts  func([]agent.ContainerMount) []agent.ContainerMount // changes what InspectMounts reports
	removeGate chan struct{}                                       // RemoveContainer waits for it
}

func (h *fakeHost) ListContainers(ctx context.Context) ([]string, error) { return h.list, h.listErr }

// InspectMounts reports what the last run asked for.
func (h *fakeHost) InspectMounts(ctx context.Context, name string) ([]agent.ContainerMount, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ms []agent.ContainerMount
	argv := h.runs[len(h.runs)-1]
	for i := 0; i+1 < len(argv); i++ {
		switch argv[i] {
		case "--mount":
			m := agent.ContainerMount{}
			for _, kv := range strings.Split(argv[i+1], ",") {
				switch {
				case strings.HasPrefix(kv, "source="):
					m.Source = strings.TrimPrefix(kv, "source=")
				case strings.HasPrefix(kv, "target="):
					m.Destination = strings.TrimPrefix(kv, "target=")
				case kv == "readonly":
					m.ReadOnly = true
				}
			}
			m.Type = "virtiofs"
			ms = append(ms, m)
		case "-v":
			p := strings.Split(argv[i+1], ":")
			ms = append(ms, agent.ContainerMount{Source: p[0], Destination: p[1], ReadOnly: len(p) > 2 && p[2] == "ro", Type: "virtiofs"})
		case "--tmpfs":
			ms = append(ms, agent.ContainerMount{Source: "tmpfs", Destination: argv[i+1], Type: "tmpfs"})
		}
	}
	if h.badMounts != nil && len(ms) > 0 {
		ms = h.badMounts(ms)
	}
	return ms, nil
}

func (h *fakeHost) RunOutput(ctx context.Context, argv []string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outputs = append(h.outputs, argv)
	if h.outputGate != nil {
		g := h.outputGate
		h.mu.Unlock()
		select {
		case <-g:
		case <-ctx.Done():
		}
		h.mu.Lock()
	}
	if h.outputErr != nil {
		return nil, h.outputErr
	}
	return []byte("[1/2] Starting container [0s]\n2.1.314 (Claude Code)\n"), nil
}

func (h *fakeHost) EnsureImage(ctx context.Context) error {
	if h.ensureHook != nil {
		h.ensureHook()
	}
	if h.block != nil {
		select {
		case <-h.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.ensureErr
}
func (h *fakeHost) ImageTag() string     { return "rubi-agent-runner:test" }
func (h *fakeHost) ContainerBin() string { return "/nonexistent-tools/container" }
func (h *fakeHost) RunDetached(ctx context.Context, argv []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runs = append(h.runs, argv)
	if h.afterRun != nil {
		defer h.afterRun()
	}
	return h.runErr
}
func (h *fakeHost) StartSessionPipe(ctx context.Context, name string, spec *agent.PipeSpec, env map[string]string, giveUp func()) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pipeErr != nil {
		return nil, h.pipeErr
	}
	h.pipes = append(h.pipes, spec)
	h.pipeEnvs = append(h.pipeEnvs, env)
	h.giveUps = append(h.giveUps, giveUp)
	return func() { h.mu.Lock(); h.pipeStops++; h.mu.Unlock() }, nil
}
func (h *fakeHost) RemoveContainer(ctx context.Context, name string) error {
	h.mu.Lock()
	g := h.removeGate
	h.mu.Unlock()
	if g != nil {
		select {
		case <-g:
		case <-ctx.Done():
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, name)
	return nil
}

func (h *fakeHost) snapshot() (runs [][]string, removed []string, stops int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]string(nil), h.runs...), append([]string(nil), h.removed...), h.pipeStops
}

// ---- a world ----

const (
	testRole = "r_aaaaaa01"
	testPort = 4411
)

type world struct {
	t       *testing.T
	dir     string // Home's directory
	project string // the role's pinned project
	cwd     string
	att     string
	be      *fakeBackend
	host    *fakeHost
	srv     *Server
	sock    string
	t3Calls chan net.Conn
	stop    func() error // stops Serve and returns its result
}

func newWorld(t *testing.T, mod ...func(*Options)) *world { return newWorldWith(t, nil, mod...) }

// newWorldWith is newWorld with a step that runs before the server starts to serve.
func newWorldWith(t *testing.T, pre func(*world), mod ...func(*Options)) *world {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := admitRetry
	admitRetry = 150 * time.Millisecond // the tests that refuse over a cap don't wait the full time
	t.Cleanup(func() { admitRetry = old })
	w := &world{t: t, dir: filepath.Join(base, "h"), project: filepath.Join(base, "proj"), att: filepath.Join(base, "t3", "attachments"),
		be: &fakeBackend{perRole: 3, total: 10}, host: &fakeHost{}, t3Calls: make(chan net.Conn, 4)}
	w.cwd = filepath.Join(w.project, "sub")
	role := filepath.Join(w.dir, "agent-roles", testRole)
	for _, d := range []string{w.dir, w.cwd, w.att, filepath.Join(role, "claude"), filepath.Join(role, "content")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.be.role = &agent.T3Role{ID: testRole, Name: "Reviewer", Project: "proj", ProjectDir: w.project,
		ClaudeDir: filepath.Join(role, "claude"), ContentDir: filepath.Join(role, "content"), MaxMode: "auto"}
	w.be.fakeAsker.res = agent.AskResult{Behavior: "deny", Message: "not in this test"}
	opt := Options{Dir: w.dir, Backend: w.be, Host: w.host, T3Port: testPort, Attachments: w.att, Claude: "/opt/claude",
		Logf: func(string, ...any) {},
		DialT3: func(ctx context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			w.t3Calls <- b
			return a, nil
		}}
	for _, m := range mod {
		m(&opt)
	}
	if pre != nil {
		pre(w)
	}
	w.srv = NewServer(opt)
	sd, err := os.MkdirTemp("", "s") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sd) })
	w.sock = filepath.Join(sd, "s.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.srv.Serve(ctx, w.sock) }()
	var once sync.Once
	var serr error
	w.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case serr = <-done:
			case <-time.After(10 * time.Second):
				serr = errors.New("Serve did not return")
			}
		})
		return serr
	}
	t.Cleanup(func() {
		if err := w.stop(); err != nil {
			t.Error(err)
		}
	})
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(w.sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return w
}

func (w *world) argv(extra ...string) []string {
	return append(t3ArgvFor(w.cwd, w.att, "bypassPermissions"), extra...)
}

// t3ArgvFor is T3's command line (see t3Argv) for this folder and attachments folder, with the MCP server it injects.
func t3ArgvFor(cwd, att, mode string) []string {
	return []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json", "--model", "m",
		"--permission-prompt-tool", "stdio", "--allowedTools", "mcp__t3-code__*", "--tools", "default", "--permission-mode", mode,
		"--mcp-config", `{"mcpServers":{"t3-code":{"type":"http","url":"http://127.0.0.1:4411/mcp"}}}`,
		"--add-dir", cwd, "--add-dir", att, "--session-id=0b0d0f4e-1111-4222-8333-444455556666"}
}

// session opens a connection, sends the request and returns the connection and Home's answer.
func (w *world) session(req StartRequest) (net.Conn, StartReply) {
	w.t.Helper()
	c, err := net.Dial("unix", w.sock)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { c.Close() })
	b, _ := json.Marshal(req)
	_, _ = c.Write(append(b, '\n')) // a refusal may come (and the connection close) before the request is written
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		w.t.Fatalf("no answer: %v", err)
	}
	var rep StartReply
	if err := json.Unmarshal(line, &rep); err != nil {
		w.t.Fatal(err)
	}
	return c, rep
}

func (w *world) start(env map[string]string, extra ...string) (net.Conn, StartReply) {
	return w.session(StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv(extra...), Env: env})
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- tests ----

func TestServerStartsASessionVMAndAnswersWithTheExec(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(map[string]string{"CLAUDE_CODE_ENTRYPOINT": "sdk-ts", "ANTHROPIC_API_KEY": "sk-ant-secret", "HOME": "/Users/x", "CLAUDE_CODE_OAUTH_TOKEN": "t"})
	defer c.Close()
	if !rep.OK || !strings.HasPrefix(rep.Container, "rubi-t3-r-aaaaaa01-") {
		t.Fatalf("reply: %+v", rep)
	}
	// the exec: the container, the env file, claude, and T3's arguments with the role's ceiling and strict MCP
	if len(rep.ExecArgv) < 12 || filepath.Base(rep.ExecArgv[0]) != "container" || rep.ExecArgv[1] != "exec" || rep.ExecArgv[len(rep.ExecArgv)-1] != "--strict-mcp-config" {
		t.Fatalf("exec: %q", rep.ExecArgv)
	}
	joined := strings.Join(rep.ExecArgv, " ")
	for _, want := range []string{"--workdir " + w.cwd, "--env-file " + rep.EnvFile, rep.Container + " /opt/claude --output-format stream-json", "--permission-mode auto"} {
		if !strings.Contains(joined, want) {
			t.Errorf("exec lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "bypassPermissions") {
		t.Errorf("the mode was not lowered to the role's ceiling:\n%s", joined)
	}
	// the env file: 0600, Home's variables and the allowed ones, none of the rest
	fi, err := os.Stat(rep.EnvFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(rep.EnvFile)
	env := string(b)
	for _, want := range []string{"CLAUDE_CODE_ENTRYPOINT=sdk-ts\n", "CLAUDE_CONFIG_DIR=" + w.be.role.ClaudeDir + "\n",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=" + filepath.Join(w.dir, "agent-signin") + "\n", "HTTPS_PROXY=http://rubi:" + strings.Repeat("p", 32) + "@127.0.0.1:3128\n", "NO_PROXY=127.0.0.1\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("env file lacks %q:\n%s", want, env)
		}
	}
	for _, bad := range []string{"sk-ant-secret", "/Users/x", "OAUTH"} {
		if strings.Contains(env, bad) {
			t.Errorf("env file holds %q", bad)
		}
	}
	// the VM: one run, with the folders, the policy files, no network, and the forwarder knows the T3 port
	runs, _, _ := w.host.snapshot()
	if len(runs) != 1 {
		t.Fatalf("runs: %q", runs)
	}
	run := strings.Join(runs[0], " ")
	for _, want := range []string{"--name " + rep.Container, "--network none", "--cap-drop ALL", "--read-only", "source=" + w.cwd + ",target=" + w.cwd + " ",
		"source=" + w.be.role.ClaudeDir, "source=" + w.att + ",target=" + w.att + ",readonly", ",readonly", "/etc/claude-code/managed-settings.json:ro", "/etc/rubi/rubi-hook.mjs:ro"} {
		if !strings.Contains(run+" ", want) {
			t.Errorf("run lacks %q:\n%s", want, run)
		}
	}
	if w.host.pipeEnvs[0]["RUBI_T3_PORT"] != "4411" {
		t.Errorf("forwarder env: %v", w.host.pipeEnvs[0])
	}
	// the files the VM sees are Home's, world-readable, not writable
	for _, f := range []string{"rubi-hook.mjs", "managed-settings.json"} {
		fi, err := os.Stat(filepath.Join(w.dir, "t3vm", f))
		if err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(w.dir, "t3vm", "rubi-hook.mjs")); !bytes.Equal(b, HookScript) {
		t.Error("the hook script in the VM is not the shipped one")
	}
	wantSettings, _ := ManagedSettings(filepath.Join(w.dir, "agent-signin"))
	if b, _ := os.ReadFile(filepath.Join(w.dir, "t3vm", "managed-settings.json")); !bytes.Equal(b, wantSettings) {
		t.Errorf("the managed settings in the VM are not the sign-in's: %s", b)
	}
	if w.srv.Sessions() != 1 {
		t.Errorf("sessions: %d", w.srv.Sessions())
	}
}

func TestServerEndsTheSessionWhenTheConnectionCloses(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	c.Close()
	eventually(t, "the session to end", func() bool { return w.srv.Sessions() == 0 })
	eventually(t, "the container to be removed", func() bool { _, rm, _ := w.host.snapshot(); return len(rm) == 1 })
	_, rm, stops := w.host.snapshot()
	if rm[0] != rep.Container || stops != 1 {
		t.Errorf("removed %q, pipe stops %d", rm, stops)
	}
	if _, err := os.Stat(rep.EnvFile); err == nil {
		t.Error("the env file is still there")
	}
	if w.be.releasedCount() != 1 {
		t.Errorf("the role was released %d times", w.be.releasedCount())
	}
	eventually(t, "the hook listener to close", func() bool {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		return !w.host.pipes[0].Hook.Offer(b)
	})
}

func TestServerHookServiceIsTheGateOfThisVM(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	w.be.fakeAsker.res = agent.AskResult{Behavior: "deny", Message: "no, from the decider"}
	a, b := net.Pipe()
	if !w.host.pipes[0].Hook.Offer(b) {
		t.Fatal("the hook listener refused a stream")
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return a, nil }}}
	resp, err := hc.Post("http://127.0.0.1:3130/ask", "application/json", strings.NewReader(goodInput))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "no, from the decider") {
		t.Fatalf("answer: %s", body)
	}
	if w.be.count() != 1 {
		t.Fatalf("asked %d times", w.be.count())
	}
	if call := w.be.calls[0]; call.Project != "proj" || call.Role != testRole || call.P.Via != "hook" {
		t.Fatalf("the ask was filed as %+v", call)
	}
}

func TestServerT3ServiceReachesHomesT3(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	a, b := net.Pipe()
	if !w.host.pipes[0].T3.Offer(b) {
		t.Fatal("the T3 listener refused a stream")
	}
	var up net.Conn
	select {
	case up = <-w.t3Calls:
	case <-time.After(5 * time.Second):
		t.Fatal("T3 was not dialled")
	}
	go func() { io.Copy(up, up) }() // T3 echoes
	a.SetDeadline(time.Now().Add(5 * time.Second))
	a.Write([]byte("mcp request"))
	got := make([]byte, 11)
	if _, err := io.ReadFull(a, got); err != nil || string(got) != "mcp request" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestServerRefusesWhatItCannotVouchFor(t *testing.T) {
	for name, tc := range map[string]struct {
		mod  func(w *world) StartRequest
		want string
	}{
		"unknown role": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: "r_bbbbbbbb", Cwd: w.cwd, Argv: w.argv()}
		}, "unknown role"},
		"folder outside the role's project": {func(w *world) StartRequest {
			other := filepath.Join(filepath.Dir(w.project), "other")
			os.MkdirAll(other, 0o700)
			return StartRequest{Op: "session.start", Role: testRole, Cwd: other, Argv: t3ArgvFor(other, w.att, "default")}
		}, "not inside the trusted project root"},
		"folder is a link out of the project": {func(w *world) StartRequest {
			link := filepath.Join(w.project, "escape")
			os.Symlink(filepath.Dir(w.project), link)
			return StartRequest{Op: "session.start", Role: testRole, Cwd: link, Argv: t3ArgvFor(link, w.att, "default")}
		}, "not inside the trusted project root"},
		"folder is Home's directory": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: testRole, Cwd: w.dir, Argv: t3ArgvFor(w.dir, w.att, "default")}
		}, "trusted project root"},
		"relative folder": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: testRole, Cwd: "proj/sub", Argv: t3ArgvFor("proj/sub", w.att, "default")}
		}, "clean absolute"},
		"settings from the command line": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv("--settings", "/tmp/x.json")}
		}, "--settings"},
		"another MCP server": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: append(w.argv(), "--mcp-config", `{"mcpServers":{"x":{"command":"sh"}}}`)}
		}, "--mcp-config"},
		"an extra folder": {func(w *world) StartRequest {
			return StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv("--add-dir", "/Users")}
		}, "--add-dir"},
		"bad op": {func(w *world) StartRequest { return StartRequest{Op: "session.stop", Role: testRole, Cwd: w.cwd} }, "does not understand"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			c, rep := w.session(tc.mod(w))
			defer c.Close()
			if rep.OK || !strings.Contains(rep.Error, tc.want) {
				t.Fatalf("reply: %+v, want an error with %q", rep, tc.want)
			}
			runs, rm, _ := w.host.snapshot()
			if len(runs) != 0 || w.srv.Sessions() != 0 {
				t.Fatalf("a VM was started for a refused request: runs %q sessions %d", runs, w.srv.Sessions())
			}
			if len(rm) != 0 {
				t.Errorf("removed %q: nothing was made", rm)
			}
		})
	}
}

func TestServerRolesBackendErrorIsRefusal(t *testing.T) {
	w := newWorld(t)
	w.be.roleErr = errors.New("role Reviewer was made for another directory than project is now")
	_, rep := w.start(nil)
	if rep.OK || !strings.Contains(rep.Error, "another directory") {
		t.Fatalf("%+v", rep)
	}
}

func TestServerCleansUpWhenAStepFails(t *testing.T) {
	for name, mod := range map[string]func(w *world){
		"image": func(w *world) { w.host.ensureErr = errors.New("no image") },
		"run":   func(w *world) { w.host.runErr = errors.New("container run failed") },
		"pipe":  func(w *world) { w.host.pipeErr = errors.New("forwarder never listened") },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			mod(w)
			_, rep := w.start(nil)
			if rep.OK || rep.Error == "" {
				t.Fatalf("%+v", rep)
			}
			if w.srv.Sessions() != 0 || w.be.releasedCount() != 1 {
				t.Errorf("sessions %d, released %d", w.srv.Sessions(), w.be.releasedCount())
			}
			runs, rm, _ := w.host.snapshot()
			if name != "image" && (len(runs) != 1 || len(rm) != 1) {
				t.Errorf("a started container must be removed: runs %d, removed %d", len(runs), len(rm))
			}
			if name == "image" && len(runs) != 0 {
				t.Errorf("the container was run without an image")
			}
			// the caps are free again
			w.host.ensureErr, w.host.runErr, w.host.pipeErr = nil, nil, nil
			if c, rep := w.start(nil); !rep.OK {
				t.Fatalf("a retry after the failure: %+v", rep)
			} else {
				c.Close()
			}
		})
	}
}

func TestServerCapsSessionsPerRoleAndOnTheComputer(t *testing.T) {
	w := newWorld(t)
	w.be.perRole, w.be.total = 2, 10
	var conns []net.Conn
	for i := 0; i < 2; i++ {
		c, rep := w.start(nil)
		if !rep.OK {
			t.Fatalf("session %d: %+v", i, rep)
		}
		conns = append(conns, c)
	}
	_, rep := w.start(nil)
	if rep.OK || !strings.Contains(rep.Error, "this role already runs 2") {
		t.Fatalf("over the per-role cap: %+v", rep)
	}
	runs, _, _ := w.host.snapshot()
	if len(runs) != 2 {
		t.Fatalf("a VM was started over the cap: %d runs", len(runs))
	}
	conns[0].Close()
	eventually(t, "a place to free up", func() bool { return w.srv.Sessions() == 1 })
	if c, rep := w.start(nil); !rep.OK {
		t.Fatalf("after a session ended: %+v", rep)
	} else {
		conns = append(conns, c)
	}
	w.be.perRole, w.be.total = 5, 2
	if _, rep := w.start(nil); rep.OK || !strings.Contains(rep.Error, "computer's session limit") {
		t.Fatalf("over the total cap: %+v", rep)
	}
	w.be.total = 0 // no cap
	if c, rep := w.start(nil); !rep.OK {
		t.Fatalf("no cap: %+v", rep)
	} else {
		conns = append(conns, c)
	}
}

func TestServerAbortsAStartWhoseConnectionWentAway(t *testing.T) {
	w := newWorld(t)
	w.host.block = make(chan struct{}) // the image build takes "forever"
	c, err := net.Dial("unix", w.sock)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv()})
	c.Write(append(b, '\n'))
	eventually(t, "the start to be waiting for the image", func() bool {
		w.srv.mu.Lock()
		defer w.srv.mu.Unlock()
		return w.srv.live == 1
	})
	c.Close()
	eventually(t, "the aborted start to release its place", func() bool {
		w.srv.mu.Lock()
		defer w.srv.mu.Unlock()
		return w.srv.live == 0 && w.srv.pending == 0
	})
	if w.be.releasedCount() != 1 {
		t.Errorf("the role was released %d times", w.be.releasedCount())
	}
	if runs, _, _ := w.host.snapshot(); len(runs) != 0 {
		t.Errorf("a container was started for a connection that was gone")
	}
}

func TestServerSocketIsPrivateAndOnlyTheOwnerIsServed(t *testing.T) {
	w := newWorld(t)
	fi, err := os.Stat(w.sock)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket: %v %v", fi, err)
	}
	// the real peer check: this process is the owner
	if c, rep := w.start(nil); !rep.OK {
		t.Fatalf("the owner was refused: %+v", rep)
	} else {
		c.Close()
	}
	// a peer that is not the owner gets nothing and nothing is started
	other := newWorld(t, func(o *Options) { o.PeerOK = func(net.Conn) bool { return false } })
	c, rep := other.start(nil)
	defer c.Close()
	if rep.OK || !strings.Contains(rep.Error, "own user") {
		t.Fatalf("%+v", rep)
	}
	if runs, _, _ := other.host.snapshot(); len(runs) != 0 {
		t.Fatal("a VM was started for a stranger")
	}
}

func TestServerRefusesRequestsThatAreNotOneJSONLine(t *testing.T) {
	w := newWorld(t)
	for name, payload := range map[string]string{
		"not json":  "hello\n",
		"too large": `{"op":"session.start","role":"` + strings.Repeat("a", maxRequest) + "\"}\n",
	} {
		c, err := net.Dial("unix", w.sock)
		if err != nil {
			t.Fatal(err)
		}
		go c.Write([]byte(payload))
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, _ := bufio.NewReader(c).ReadBytes('\n')
		var rep StartReply
		if json.Unmarshal(line, &rep) != nil || rep.OK || rep.Error == "" {
			t.Errorf("%s: got %q", name, line)
		}
		c.Close()
	}
	if runs, _, _ := w.host.snapshot(); len(runs) != 0 {
		t.Fatal("a VM was started")
	}
}

func TestServerEndsSessionsWhenHomeStops(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	if err := w.stop(); err != nil {
		t.Fatal(err)
	}
	if _, rm, _ := w.host.snapshot(); len(rm) != 1 || rm[0] != rep.Container {
		t.Errorf("the VM was not removed: %q", rm)
	}
	if _, err := os.Stat(w.sock); err == nil {
		t.Error("the socket file is left behind")
	}
	if _, err := os.Stat(rep.EnvFile); err == nil {
		t.Error("the env file is left behind")
	}
	if w.srv.Sessions() != 0 || w.be.releasedCount() != 1 {
		t.Errorf("sessions %d, released %d", w.srv.Sessions(), w.be.releasedCount())
	}
}

func TestNetpipeServiceIDsMatch(t *testing.T) {
	// the VM's forwarder names the hook service 3 and T3's 4 (runner/container/rubi-netpipe); the pipe supervisor
	// offers them to PipeSpec.Hook and PipeSpec.T3
	if netpipe.SvcHook != 3 || netpipe.SvcT3 != 4 {
		t.Fatal("service ids changed")
	}
}

// A session VM has no auth gateway; the gateway port only exists when a test asks for a scripted API.
func TestServerGatewayServiceExistsOnlyForTests(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK || w.host.pipes[0].Gateway != nil {
		t.Fatalf("%+v gateway %v", rep, w.host.pipes[0].Gateway)
	}
	w2 := newWorld(t, func(o *Options) {
		o.testAPI = func(context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			go func() { io.Copy(b, b); b.Close() }()
			return a, nil
		}
		o.testEnv = map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:3129"}
	})
	c2, rep2 := w2.start(nil)
	defer c2.Close()
	if !rep2.OK || w2.host.pipes[0].Gateway == nil {
		t.Fatalf("%+v", rep2)
	}
	b, _ := os.ReadFile(rep2.EnvFile)
	if !strings.Contains(string(b), "ANTHROPIC_BASE_URL=http://127.0.0.1:3129\n") {
		t.Errorf("env: %s", b)
	}
	a, bb := net.Pipe()
	if !w2.host.pipes[0].Gateway.Offer(bb) {
		t.Fatal("refused")
	}
	a.SetDeadline(time.Now().Add(5 * time.Second))
	a.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(a, got); err != nil || string(got) != "ping" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestServerAnswersTheVersionProbeWithoutASession(t *testing.T) {
	w := newWorld(t)
	probe := func(role string, argv ...string) StartReply {
		c, rep := w.session(StartRequest{Op: OpVersion, Role: role, Cwd: w.dir, Argv: argv})
		c.Close()
		return rep
	}
	rep := probe(testRole, "--version")
	if !rep.OK || rep.Output != "2.1.314 (Claude Code)\n" {
		t.Fatalf("%+v", rep)
	}
	// the throwaway VM: no network, no mounts, no capabilities, the image's claude
	w.host.mu.Lock()
	run := strings.Join(w.host.outputs[0], " ")
	w.host.mu.Unlock()
	for _, want := range []string{"--rm", "--network none", "--cap-drop ALL", "--read-only", "--entrypoint /opt/claude", "rubi-agent-runner:test --version"} {
		if !strings.Contains(run, want) {
			t.Errorf("version VM lacks %q: %s", want, run)
		}
	}
	if strings.Contains(run, "--mount") || strings.Contains(run, " -v ") {
		t.Errorf("the version VM mounts something: %s", run)
	}
	// asked again: the answer is kept
	if rep := probe(testRole, "--version"); !rep.OK || rep.Output != "2.1.314 (Claude Code)\n" {
		t.Fatalf("%+v", rep)
	}
	w.host.mu.Lock()
	n := len(w.host.outputs)
	w.host.mu.Unlock()
	if n != 1 {
		t.Errorf("the image was asked %d times", n)
	}
	if runs, _, _ := w.host.snapshot(); len(runs) != 0 || w.srv.Sessions() != 0 {
		t.Errorf("a probe started a session: %d runs, %d sessions", len(runs), w.srv.Sessions())
	}
	// only --version, and only for a role that exists
	if rep := probe(testRole, "--version", "--verbose"); rep.OK || !strings.Contains(rep.Error, "only answers --version") {
		t.Errorf("%+v", rep)
	}
	if rep := probe(testRole, "-p", "hi"); rep.OK {
		t.Errorf("%+v", rep)
	}
	if rep := probe("r_bbbbbbbb", "--version"); rep.OK || !strings.Contains(rep.Error, "unknown role") {
		t.Errorf("%+v", rep)
	}
	w2 := newWorld(t)
	w2.host.outputErr = errors.New("boom")
	c, rep := w2.session(StartRequest{Op: OpVersion, Role: testRole, Argv: []string{"--version"}})
	c.Close()
	if rep.OK || !strings.Contains(rep.Error, "boom") {
		t.Errorf("%+v", rep)
	}
}
