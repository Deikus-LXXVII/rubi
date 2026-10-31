package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// fakeCmd answers commands from a function; no real command ever runs.
type fakeCmd struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(argv []string) ([]byte, error)
}

func (f *fakeCmd) Run(ctx context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.fn(argv)
}

func (f *fakeCmd) Stream(ctx context.Context, argv []string, line func(string)) error {
	out, err := f.Run(ctx, argv)
	for _, l := range strings.Split(string(out), "\n") {
		line(l)
	}
	return err
}

func (f *fakeCmd) ran(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			n++
		}
	}
	return n
}

// machine is a canned Mac: what each probe command prints.
type machine struct {
	version  string // output of container --version ("" = not installed)
	sysJSON  string // output of container system status --format json ("" = fails)
	network  string // output of network inspect ("" = missing)
	image    bool
	started  bool // system start was run
	netMade  bool
	built    bool
	extra    func(argv []string) ([]byte, error, bool)
	settings Settings
}

const sysRunning = `{"client":{"appName":"container","build":"release","commit":"aa8d76b44a834eee3ebf6a0bc577e376f65f6361","version":"1.6.1"},"host":{"architecture":"arm64","cpus":11,"operatingSystem":"Version 27.0 (Build 26A428)"},"paths":{"appRoot":"%APP%","installRoot":"/usr/local/"},"resources":{"containersRunning":1,"containersTotal":1,"images":5},"server":{"appName":"container-apiserver","build":"release","commit":"aa8d76b44a834eee3ebf6a0bc577e376f65f6361","version":"1.6.1"},"status":"running"}`

const version161 = "container CLI version 1.6.1 (build: release, commit: aa8d76b)\n"

func (m *machine) fn(argv []string) ([]byte, error) {
	if m.extra != nil {
		if out, err, ok := m.extra(argv); ok {
			return out, err
		}
	}
	j := strings.Join(argv, " ")
	switch {
	case j == "container --version":
		if m.version == "" {
			return nil, errors.New("not found")
		}
		return []byte(m.version), nil
	case strings.HasPrefix(j, "container system status"):
		if m.sysJSON == "" {
			return []byte("apiserver is not running"), errors.New("exit status 1")
		}
		return []byte(m.sysJSON), nil
	case strings.HasPrefix(j, "container system start"):
		m.started = true
		return []byte("starting"), nil
	case strings.HasPrefix(j, "container network inspect"):
		if m.network == "" {
			return []byte("[]"), errors.New("not found")
		}
		return []byte(m.network), nil
	case strings.HasPrefix(j, "container network create"):
		m.netMade = true
		return nil, nil
	case strings.HasPrefix(j, "container image inspect"):
		if !m.image {
			return nil, errors.New("not found")
		}
		return []byte("[{}]"), nil
	case strings.HasPrefix(j, "container build"):
		m.built = true
		m.image = true
		return []byte("#1 [builder 1/4] FROM node\n#2 DONE\n"), nil
	}
	return nil, errors.New("unexpected command: " + j)
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func internalNet(t *testing.T) string {
	return readFile(t, "../agent/testdata/container-1.6.1-network-internal.json")
}
func defaultNet(t *testing.T) string {
	return readFile(t, "../agent/testdata/container-1.6.1-network-default.json")
}

// fakeAgents has projects and a scripted sign-in.
type fakeAgents struct {
	mu       sync.Mutex
	projects []agent.Project
	running  map[string]bool
	box      *fakeBox
	selftest *fakeSelfTest
	sandboxN int
}

func (a *fakeAgents) Projects() []agent.Project { return a.projects }
func (a *fakeAgents) VMRunning(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running[id]
}
func (a *fakeAgents) Sandbox(ctx context.Context, id string) (Box, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sandboxN++
	return a.box, nil
}
func (a *fakeAgents) BeginSelfTest(id string) (SelfTestRun, error) {
	if a.selftest == nil {
		return nil, errors.New("an agent session is running or waiting")
	}
	return a.selftest, nil
}

// fakeBox runs scripted processes in the "VM".
type fakeBox struct {
	mu        sync.Mutex
	loggedIn  bool
	login     func(c *fakeConn) // plays `claude auth login`
	probeOut  string
	execs     [][]string
	envs      []map[string]string
	lastLogin *fakeConn
}

func (b *fakeBox) Exec(ctx context.Context, argv []string, extra map[string]string) (agent.Conn, error) {
	b.mu.Lock()
	b.execs = append(b.execs, argv)
	b.envs = append(b.envs, extra)
	b.mu.Unlock()
	c := newFakeConn()
	switch {
	case len(argv) >= 3 && argv[1] == "auth" && argv[2] == "status":
		b.mu.Lock()
		in := b.loggedIn
		b.mu.Unlock()
		out := `{"loggedIn":false}`
		if in {
			out = `{"loggedIn":true,"authMethod":"claude.ai"}`
		}
		go func() { c.out.Write([]byte(out)); c.finish() }()
	case len(argv) >= 3 && argv[1] == "auth" && argv[2] == "login":
		b.mu.Lock()
		b.lastLogin = c
		b.mu.Unlock()
		go b.login(c)
	case len(argv) >= 1 && argv[0] == "node":
		go func() { c.out.Write([]byte(b.probeOut)); c.finish() }()
	}
	return c, nil
}

type fakeConn struct {
	out    *io.PipeWriter
	in     *io.PipeReader
	r      *io.PipeReader
	w      *io.PipeWriter
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	stdin  bytes.Buffer
	closed bool
}

func newFakeConn() *fakeConn {
	r, w := io.Pipe()
	c := &fakeConn{r: r, out: w, done: make(chan struct{})}
	return c
}

func (c *fakeConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.ErrClosedPipe
	}
	return c.stdin.Write(p)
}
func (c *fakeConn) CloseWrite() error     { return nil }
func (c *fakeConn) Close() error          { c.finish(); return nil }
func (c *fakeConn) Done() <-chan struct{} { return c.done }
func (c *fakeConn) finish() {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.out.Close()
		close(c.done)
	})
}
func (c *fakeConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
func (c *fakeConn) stdinText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdin.String()
}

type fakeSelfTest struct {
	box   *fakeBox
	turn  agent.TurnResult
	err   error
	ended bool
	mu    sync.Mutex
}

func (f *fakeSelfTest) Sandbox(ctx context.Context) (Box, error) { return f.box, nil }
func (f *fakeSelfTest) Turn(ctx context.Context, prompt string, d time.Duration) (agent.TurnResult, error) {
	return f.turn, f.err
}
func (f *fakeSelfTest) End() { f.mu.Lock(); f.ended = true; f.mu.Unlock() }

// env is a Setup on a fake machine.
type testEnv struct {
	t      *testing.T
	s      *Setup
	m      *machine
	cmd    *fakeCmd
	ag     *fakeAgents
	dir    string
	appDir string
	cfg    Settings
}

func newTestEnv(t *testing.T, mods ...func(*Options)) *testEnv {
	t.Helper()
	root := t.TempDir()
	e := &testEnv{t: t, m: &machine{}, ag: &fakeAgents{running: map[string]bool{}, box: &fakeBox{}}, dir: filepath.Join(root, "helper"), appDir: filepath.Join(root, "app")}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.m.extra = func([]string) ([]byte, error, bool) { return nil, nil, false }
	e.cmd = &fakeCmd{fn: e.m.fn}
	opt := Options{Dir: e.dir, Agents: e.ag, Cmd: e.cmd, GOOS: "darwin", SystemTrees: []string{},
		Settings: func() Settings { return e.cfg },
		Update: func(fn func(*Settings)) error {
			fn(&e.cfg)
			return nil
		}}
	for _, f := range mods {
		f(&opt)
	}
	e.s = New(opt)
	return e
}

// ready makes steps 1 to 4 done on the fake machine.
func (e *testEnv) ready() {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.appDir, "kernels"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.appDir, "kernels", "default.kernel-arm64"), []byte("k"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.m.version = version161
	e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", e.appDir+"/")
	e.m.network = internalNet(e.t)
	e.m.image = true
}

func (e *testEnv) addProject(name string) agent.Project {
	e.t.Helper()
	dir := filepath.Join(e.t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	p := agent.Project{ID: name, Dir: real}
	e.ag.projects = append(e.ag.projects, p)
	e.cfg.Projects = append(e.cfg.Projects, real)
	return p
}

func (e *testEnv) step(id string) Step {
	e.t.Helper()
	for _, st := range e.s.Status(context.Background()).Steps {
		if st.ID == id {
			return st
		}
	}
	e.t.Fatalf("no step %s", id)
	return Step{}
}

// wait waits for the running job to end.
func (e *testEnv) wait() {
	e.t.Helper()
	for i := 0; i < 500; i++ {
		if !e.s.Busy() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("the job didn't end")
}
