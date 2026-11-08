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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// fakeStreamHost stands in for the microVM backend's session host.
type fakeStreamHost struct {
	mu       sync.Mutex
	readyErr error
	startErr error
	execErr  error         // given to every VM it starts
	gate     chan struct{} // when set, a start waits for it (or for its context)
	verOut   string        // what claude --version prints ("HANG": it never answers)
	late     time.Duration // when set, the session's process writes its output only after this long
	vms      []*fakeSessionVM
}

func (h *fakeStreamHost) Ready() error { h.mu.Lock(); defer h.mu.Unlock(); return h.readyErr }
func (h *fakeStreamHost) StartSessionVM(ctx context.Context, name string, mounts []agent.Mount) (agent.SessionVM, error) {
	h.mu.Lock()
	gate := h.gate
	h.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		return nil, h.startErr
	}
	if h.readyErr != nil { // the real host checks Ready as it starts a VM
		return nil, h.readyErr
	}
	out := "VERSION 9.9\n"
	if h.verOut != "" {
		out = h.verOut
	}
	vm := &fakeSessionVM{name: name, mounts: mounts, code: 5, out: out, execErr: h.execErr, late: h.late}
	h.vms = append(h.vms, vm)
	return vm, nil
}
func (h *fakeStreamHost) all() []*fakeSessionVM {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.vms)
}

type fakeSessionVM struct {
	mu      sync.Mutex
	name    string
	mounts  []agent.Mount
	pipes   int
	stopped bool
	execs   []fakeExec
	code    int
	out     string
	execErr error
	late    time.Duration
	procs   []*fakeProc
	hook    *netpipe.Listener
	giveUp  func()
}

type fakeExec struct {
	dir  string
	env  map[string]string
	argv []string
}

func (v *fakeSessionVM) StartPipe(ctx context.Context, spec *agent.PipeSpec, env map[string]string, giveUp func()) (func(), error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if spec == nil || spec.Proxy == nil || spec.Hook == nil || spec.T3 == nil {
		return nil, errors.New("a pipe without its services")
	}
	v.pipes++
	v.hook, v.giveUp = spec.Hook, giveUp
	return func() {}, nil
}

// ExecIn: claude --version prints out; anything else echoes stdin upper-cased and ends with code.
func (v *fakeSessionVM) ExecIn(ctx context.Context, dir string, env map[string]string, argv []string, stderr io.Writer) (agent.SessionProc, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.execErr != nil {
		return nil, v.execErr
	}
	v.execs = append(v.execs, fakeExec{dir, env, argv})
	if stderr == nil {
		stderr = io.Discard
	}
	if argv[0] == "hang" {
		return &hangProc{closed: make(chan struct{})}, nil
	}
	if slices.Contains(argv, "--version") {
		if v.out == "HANG" {
			return &hangProc{closed: make(chan struct{})}, nil
		}
		return &versionProc{out: strings.NewReader(v.out)}, nil
	}
	if v.late > 0 {
		lp := &lateProc{delay: v.late, out: "LATE"}
		return lp, nil
	}
	fp := newFakeProc(v.code, stderr)
	v.procs = append(v.procs, fp)
	return sessionProc{fp}, nil
}
func (v *fakeSessionVM) Stop(context.Context) error {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	return nil
}
func (v *fakeSessionVM) snap() (stopped bool, execs []fakeExec) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopped, slices.Clone(v.execs)
}

type sessionProc struct{ *fakeProc }

func (sessionProc) Done() <-chan struct{} { return make(chan struct{}) }

// versionProc prints out and ends with 0.
type versionProc struct{ out io.Reader }

func (p *versionProc) Read(b []byte) (int, error) { return p.out.Read(b) }
func (*versionProc) Write(b []byte) (int, error)  { return len(b), nil }
func (*versionProc) CloseWrite() error            { return nil }
func (*versionProc) Close() error                 { return nil }
func (*versionProc) Wait() (int, error)           { return 0, nil }
func (*versionProc) Done() <-chan struct{}        { return make(chan struct{}) }

func streamWorld(t *testing.T) (*world, *fakeStreamHost) { return streamWorldIdle(t, 50*time.Millisecond) }

// streamWorldIdle is streamWorld whose role VMs stop idle after idle (restored after the server has stopped).
func streamWorldIdle(t *testing.T, idle time.Duration) (*world, *fakeStreamHost) {
	old := roleVMIdle
	roleVMIdle = idle
	t.Cleanup(func() { roleVMIdle = old })
	h := &fakeStreamHost{}
	w := newWorld(t, func(o *Options) { o.Host, o.Stream, o.StreamClaude = nil, h, "/opt/sdk/claude" })
	return w, h
}

func (w *world) runShim(stdin string, env []string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	o := ClientOptions{ExecTool: "container", Stdin: strings.NewReader(stdin),
		Getwd:   func() (string, error) { return w.cwd, nil },
		Environ: func() []string { return env },
		Exec: func(string, []string, []string, *os.File) error {
			w.t.Error("a streamed session ran a tool")
			return errors.New("no")
		}}
	code := Run(w.sock, testRole, args, &stdout, &stderr, o)
	return code, stdout.String(), stderr.String()
}

func TestStreamedSessionRunsClaudeInItsVM(t *testing.T) {
	w, h := streamWorld(t)
	code, out, errOut := w.runShim("hi", []string{"ANTHROPIC_MODEL=m", "GITHUB_TOKEN=secret"}, w.argv()...)
	if code != 5 || out != "HI" || errOut != "warn" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	vms := h.all()
	if len(vms) != 1 {
		t.Fatalf("%d VMs", len(vms))
	}
	vm := vms[0]
	eventually(t, "the VM stopped", func() bool { s, _ := vm.snap(); return s })
	if !strings.HasPrefix(vm.name, ContainerPrefix) || vm.pipes != 1 {
		t.Errorf("name %q, pipes %d", vm.name, vm.pipes)
	}
	// the folders: the workspace read-write at its own path, the role's, the policy folders read-only, nothing else
	// the role's VM: the project's root read-write (the session runs in its own folder in it), the role's folders,
	// T3's attachments, the policy folders read-only, nothing else
	want := map[string]bool{w.project: false, w.be.role.ClaudeDir: false, w.be.role.ContentDir: true, w.att: true,
		filepath.Join(w.dir, "agent-signin"): false, "/etc/claude-code": true, "/etc/rubi": true}
	got := map[string]bool{}
	for _, m := range vm.mounts {
		got[m.Target] = m.ReadOnly
		if m.Target == "/etc/claude-code" || m.Target == "/etc/rubi" {
			if !strings.HasPrefix(m.Source, filepath.Join(w.dir, "t3vm", "vm-etc")) {
				t.Errorf("policy folder %s from %s", m.Target, m.Source)
			}
			if ents, _ := os.ReadDir(m.Source); len(ents) != 1 {
				t.Errorf("policy folder %s holds %d files", m.Source, len(ents))
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}
	for k, ro := range want {
		if g, ok := got[k]; !ok || g != ro {
			t.Errorf("mount %s: present %v read-only %v, want read-only %v", k, ok, g, ro)
		}
	}
	_, execs := vm.snap()
	if len(execs) != 1 {
		t.Fatalf("%d execs", len(execs))
	}
	e := execs[0]
	if e.dir != w.cwd || e.argv[0] != "/opt/sdk/claude" || !slices.Contains(e.argv, "--output-format") {
		t.Errorf("exec %q in %s", e.argv, e.dir)
	}
	if e.env["GITHUB_TOKEN"] != "" || e.env["CLAUDE_CONFIG_DIR"] != w.be.role.ClaudeDir || !strings.HasPrefix(e.env["HTTPS_PROXY"], "http://rubi:") {
		t.Errorf("env %v", e.env)
	}
	eventually(t, "the role released", func() bool { return w.be.releasedCount() >= 1 })
}

func TestStreamedSessionRefusals(t *testing.T) {
	w, h := streamWorld(t)
	h.readyErr = errors.New("only 300 MiB of memory are free")
	if code, _, errOut := w.runShim("", nil, w.argv()...); code == 0 || !strings.Contains(errOut, "300 MiB") {
		t.Errorf("not ready: %d %q", code, errOut)
	}
	h.readyErr = nil
	// a folder outside the role's project
	outside := t.TempDir()
	w.cwd = outside
	if code, _, errOut := w.runShim("", nil, t3ArgvFor(outside, w.att, "bypassPermissions")...); code == 0 || errOut == "" {
		t.Errorf("outside the project: %d %q", code, errOut)
	}
	if len(h.all()) != 0 {
		t.Error("a VM was started for a refused session")
	}
}

func TestStreamedSessionStopsTheVMWhenClaudeDoesntStart(t *testing.T) {
	w, h := streamWorld(t)
	h.mu.Lock()
	h.execErr = errors.New("no such file")
	h.mu.Unlock()
	code, _, errOut := w.runShim("x", nil, w.argv()...)
	if code == 0 || !strings.Contains(errOut, "didn't start in the VM") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	vm := h.all()[0]
	eventually(t, "the VM stopped", func() bool { s, _ := vm.snap(); return s })
}

func TestStreamedVersionAsksAThrowawayVM(t *testing.T) {
	w, h := streamWorld(t)
	code, out, _ := w.runShim("", nil, "--version")
	if code != 0 || out != "VERSION 9.9\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	vms := h.all()
	if len(vms) != 1 || len(vms[0].mounts) != 0 {
		t.Fatalf("version VMs %d (mounts %v)", len(vms), vms[0].mounts)
	}
	if s, ex := vms[0].snap(); !s || len(ex) != 1 || !slices.Equal(ex[0].argv, []string{"/opt/sdk/claude", "--version"}) {
		t.Errorf("stopped %v, execs %v", s, ex)
	}
	// remembered: a second ask starts no VM
	if code, out, _ := w.runShim("", nil, "--version"); code != 0 || out != "VERSION 9.9\n" || len(h.all()) != 1 {
		t.Errorf("second ask: %d %q, %d VMs", code, out, len(h.all()))
	}
}

func TestClaudeForArch(t *testing.T) {
	if !strings.HasSuffix(claudeForArch("amd64"), "claude-agent-sdk-linux-x64/claude") || !strings.HasSuffix(claudeForArch("arm64"), "claude-agent-sdk-linux-arm64/claude") {
		t.Error(claudeForArch("amd64"), claudeForArch("arm64"))
	}
}

func TestSessionSpecMountsKeepThePolicyApart(t *testing.T) {
	w, _ := streamWorld(t)
	files, err := w.srv.writeFiles()
	if err != nil {
		t.Fatal(err)
	}
	spec := SessionSpec{Name: "rubi-t3-x", Role: testRole, ClaudeDir: w.be.role.ClaudeDir, ContentDir: w.be.role.ContentDir,
		Cwd: w.cwd, TrustedRoot: w.project, HelperDir: w.dir, SignInDir: files.signIn, HookScript: files.hook,
		ManagedSettings: files.settings, T3Port: testPort}
	pol, err := w.srv.policyDirs(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Mounts(pol); err != nil {
		t.Fatalf("the real policy folders: %v", err)
	}
	if _, err := spec.Mounts([]agent.Mount{{Source: pol[0].Source, Target: pol[0].Target}}); err == nil {
		t.Error("a read-write policy folder was accepted")
	}
	// a policy folder inside a read-write folder of the session (the role's claude directory) would be the VM's to change
	inside := filepath.Join(w.be.role.ClaudeDir, "policy")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Mounts([]agent.Mount{{Source: inside, Target: "/etc/rubi", ReadOnly: true}}); err == nil {
		t.Error("a policy folder inside a read-write folder was accepted")
	}
	bad := spec
	bad.T3Port = 3129
	if _, err := bad.Mounts(pol); err == nil {
		t.Error("a forwarder port was accepted as T3's")
	}
}

// lateProc ignores stdin, writes out after delay, then ends with 0.
type lateProc struct {
	delay time.Duration
	out   string
	once  sync.Once
	r     io.Reader
	done  chan struct{}
}

func (p *lateProc) Read(b []byte) (int, error) {
	p.once.Do(func() { time.Sleep(p.delay); p.r = strings.NewReader(p.out) })
	return p.r.Read(b)
}
func (*lateProc) Write(b []byte) (int, error) { return len(b), nil }
func (*lateProc) CloseWrite() error           { return nil }
func (*lateProc) Close() error                { return nil }
func (*lateProc) Wait() (int, error)          { return 0, nil }
func (*lateProc) Done() <-chan struct{}       { return make(chan struct{}) }

// The answer's write deadline doesn't bound the session: output that comes later still reaches the shim.
func TestStreamedSessionOutputHasNoDeadline(t *testing.T) {
	old := replyLimit
	replyLimit = 50 * time.Millisecond
	t.Cleanup(func() { replyLimit = old })
	w, h := streamWorld(t)
	h.late = 300 * time.Millisecond
	if code, out, errOut := w.runShim("", nil, w.argv()...); code != 0 || out != "LATE" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// Home stopping ends a streamed session's process and Serve returns.
func TestStreamedSessionEndsWhenHomeStops(t *testing.T) {
	w, h := streamWorld(t)
	done := make(chan int, 1)
	go func() {
		// stdin that never ends: the process runs until something ends it
		pr, _ := io.Pipe()
		var stdout, stderr bytes.Buffer
		o := ClientOptions{ExecTool: "container", Stdin: pr, Getwd: func() (string, error) { return w.cwd, nil },
			Environ: func() []string { return nil }}
		done <- Run(w.sock, testRole, w.argv(), &stdout, &stderr, o)
	}()
	eventually(t, "the process started", func() bool {
		vms := h.all()
		if len(vms) == 0 {
			return false
		}
		vms[0].mu.Lock()
		defer vms[0].mu.Unlock()
		return len(vms[0].procs) == 1
	})
	if err := w.stop(); err != nil {
		t.Fatal(err)
	}
	vm := h.all()[0]
	vm.mu.Lock()
	p := vm.procs[0]
	vm.mu.Unlock()
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if !closed {
		t.Error("the process runs on after Home stopped")
	}
	if s, _ := vm.snap(); !s {
		t.Error("the VM runs on after Home stopped")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the shim was left waiting")
	}
}

// A shim that leaves while its VM starts ends the start: the VM is not left running and nothing is executed.
func TestStreamedStartEndsWhenTheShimLeaves(t *testing.T) {
	w, h := streamWorld(t)
	h.gate = make(chan struct{})
	c, err := net.Dial("unix", w.sock)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv()})
	_, _ = c.Write(append(b, '\n'))
	time.Sleep(200 * time.Millisecond) // the start waits at the gate
	c.Close()
	eventually(t, "the role released", func() bool { return w.be.releasedCount() >= 1 })
	if vms := h.all(); len(vms) != 0 {
		t.Errorf("%d VMs started for a shim that left", len(vms))
	}
}

// Data before the answer is not the protocol: the start is ended.
func TestStreamedStartRefusesEarlyData(t *testing.T) {
	w, h := streamWorld(t)
	h.gate = make(chan struct{})
	c, err := net.Dial("unix", w.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b, _ := json.Marshal(StartRequest{Op: "session.start", Role: testRole, Cwd: w.cwd, Argv: w.argv()})
	_, _ = c.Write(append(append(b, '\n'), 'x'))
	eventually(t, "the role released", func() bool { return w.be.releasedCount() >= 1 })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(c).ReadBytes('\n')
	var rep StartReply
	_ = json.Unmarshal(line, &rep)
	if rep.OK {
		t.Error("a session started after early data")
	}
}

func TestClientRefusesAStreamWithACommand(t *testing.T) {
	home := newFakeHome(t, StartReply{OK: true, Stream: true, ExecArgv: []string{"container", "exec", "x"}})
	var stderr bytes.Buffer
	o := ClientOptions{ExecTool: "container", Getwd: func() (string, error) { return "/w", nil }, Environ: func() []string { return nil },
		Exec: func(string, []string, []string, *os.File) error { t.Error("ran a tool"); return errors.New("no") }}
	if code := Run(home.sock, testRole, []string{"-p"}, io.Discard, &stderr, o); code == 0 {
		t.Error("a stream answer with a command was taken")
	}
}

// hangProc never prints or ends until it is closed.
type hangProc struct {
	once   sync.Once
	closed chan struct{}
}

func (p *hangProc) Read([]byte) (int, error)  { <-p.closed; return 0, io.EOF }
func (*hangProc) Write(b []byte) (int, error) { return len(b), nil }
func (*hangProc) CloseWrite() error           { return nil }
func (p *hangProc) Close() error              { p.once.Do(func() { close(p.closed) }); return nil }
func (p *hangProc) Wait() (int, error)        { <-p.closed; return -1, nil }
func (p *hangProc) Done() <-chan struct{}     { return p.closed }

// A claude --version that never answers doesn't hold the probe past its time: it fails, and its VM is stopped.
func TestStreamedVersionProbeHasADeadline(t *testing.T) {
	old := versionLimit
	versionLimit = 300 * time.Millisecond
	t.Cleanup(func() { versionLimit = old })
	w, h := streamWorld(t)
	h.verOut = "HANG"
	start := time.Now()
	if code, _, _ := w.runShim("", nil, "--version"); code == 0 {
		t.Error("a hung version probe answered")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the version probe outlived its limit")
	}
	vm := h.all()[0]
	eventually(t, "the probe's VM stopped", func() bool { s, _ := vm.snap(); return s })
}

// The self-test's probe in a streamed session VM: the command runs in the project folder; one that never ends is ended
// at the probe's limit, and the VM is stopped either way.
func TestStreamedProbe(t *testing.T) {
	w, h := streamWorld(t)
	if _, err := w.srv.Probe(context.Background(), testRole, []string{"sh"}); err == nil || !strings.Contains(err.Error(), "ended with 5") {
		t.Errorf("a failing command: %v", err)
	}
	vm := h.all()[0]
	if _, ex := vm.snap(); len(ex) != 1 || ex[0].dir != w.project || ex[0].argv[0] != "sh" {
		t.Errorf("execs %+v", ex)
	}
	old := probeLimit
	probeLimit = 300 * time.Millisecond
	t.Cleanup(func() { probeLimit = old })
	start := time.Now()
	if _, err := w.srv.Probe(context.Background(), testRole, []string{"hang"}); err == nil {
		t.Error("a hung probe succeeded")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the probe outlived its limit")
	}
	if len(h.all()) != 1 {
		t.Errorf("%d VMs: the probes didn't share the role's VM", len(h.all()))
	}
	eventually(t, "the role's VM stopped after its idle time", func() bool { s, _ := vm.snap(); return s })
}

// shimSession runs a shim whose stdin stays open until the returned func is called; it returns the exit status channel.
func (w *world) shimSession(sessionID, thread string) (end func(), done chan int) {
	pr, pw := io.Pipe()
	done = make(chan int, 1)
	argv := t3ArgvFor(w.cwd, w.att, "bypassPermissions")
	for i, a := range argv {
		if strings.HasPrefix(a, "--session-id=") {
			argv[i] = "--session-id=" + sessionID
		}
	}
	go func() {
		var stdout, stderr bytes.Buffer
		o := ClientOptions{ExecTool: "container", Stdin: pr, Getwd: func() (string, error) { return w.cwd, nil },
			Environ: func() []string { return []string{"T3CODE_THREAD_ID=" + thread} }}
		done <- Run(w.sock, testRole, argv, &stdout, &stderr, o)
	}()
	return func() { pw.Close() }, done
}

func (v *fakeSessionVM) procCount() int { v.mu.Lock(); defer v.mu.Unlock(); return len(v.procs) }

// hookAsk sends a PreToolUse input with sessionID to the VM's hook gate and returns the session the ask was filed under.
func hookAsk(t *testing.T, w *world, vm *fakeSessionVM, sessionID string) string {
	t.Helper()
	vm.mu.Lock()
	hook := vm.hook
	vm.mu.Unlock()
	a, b := net.Pipe()
	if !hook.Offer(b) {
		t.Fatal("the hook service is closed")
	}
	body := `{"hook_event_name":"PreToolUse","session_id":"` + sessionID + `","tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"tu1","cwd":"` + w.cwd + `"}`
	req, _ := http.NewRequest("POST", "http://127.0.0.1:3130/ask", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	go func() { _ = req.Write(a) }()
	resp, err := http.ReadResponse(bufio.NewReader(a), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	a.Close()
	w.be.mu.Lock()
	defer w.be.mu.Unlock()
	calls := w.be.calls
	return calls[len(calls)-1].P.SessionID
}

// Sessions of one role share its VM; the gate files each ask under the session's T3 thread; the VM stops only after its
// last session, and the next session gets a new one.
func TestRoleVMIsSharedAndStopsAfterItsLastSession(t *testing.T) {
	const idle = 300 * time.Millisecond
	w, h := streamWorldIdle(t, idle)
	endA, doneA := w.shimSession("sess-a", "thread-a")
	eventually(t, "the first session runs", func() bool { vms := h.all(); return len(vms) == 1 && vms[0].procCount() == 1 })
	endB, doneB := w.shimSession("sess-b", "thread-b")
	vm := h.all()[0]
	eventually(t, "the second session runs in the same VM", func() bool { return vm.procCount() == 2 })
	if len(h.all()) != 1 {
		t.Fatalf("%d VMs for one role", len(h.all()))
	}
	if got := hookAsk(t, w, vm, "sess-a"); got != "thread-a" {
		t.Errorf("an ask of session a was filed under %q", got)
	}
	if got := hookAsk(t, w, vm, "sess-b"); got != "thread-b" {
		t.Errorf("an ask of session b was filed under %q", got)
	}
	if got := hookAsk(t, w, vm, "someone-else"); got != "someone-else" {
		t.Errorf("an unknown session id was filed under %q", got)
	}
	endA()
	<-doneA
	time.Sleep(2 * idle)
	if s, _ := vm.snap(); s {
		t.Fatal("the VM stopped while a session still ran")
	}
	if got := hookAsk(t, w, vm, "sess-a"); got != "sess-a" {
		t.Errorf("an ended session's id still names its thread: %q", got)
	}
	endB()
	<-doneB
	eventually(t, "the VM stopped after its last session", func() bool { s, _ := vm.snap(); return s })
	endC, doneC := w.shimSession("sess-c", "thread-c")
	eventually(t, "a new VM for the next session", func() bool { vms := h.all(); return len(vms) == 2 && vms[1].procCount() == 1 })
	endC()
	<-doneC
}

// A claude session id already running in the role's VM is refused (two sessions can't share a thread's id).
func TestRoleVMRefusesARunningSessionID(t *testing.T) {
	w, h := streamWorld(t)
	end, done := w.shimSession("dup", "t1")
	defer func() { end(); <-done }()
	eventually(t, "the session runs", func() bool { vms := h.all(); return len(vms) == 1 && vms[0].procCount() == 1 })
	_, done2 := w.shimSession("dup", "t2")
	if code := <-done2; code == 0 {
		t.Error("a second session with the same id started")
	}
}

// The VM's pipe giving up ends every session in it and the VM; the next session gets a new VM.
func TestRoleVMPipeFailureEndsItsSessions(t *testing.T) {
	w, h := streamWorld(t)
	_, doneA := w.shimSession("a", "t1")
	_, doneB := w.shimSession("b", "t2")
	eventually(t, "both sessions run", func() bool { vms := h.all(); return len(vms) == 1 && vms[0].procCount() == 2 })
	vm := h.all()[0]
	vm.mu.Lock()
	giveUp := vm.giveUp
	vm.mu.Unlock()
	giveUp()
	for _, d := range []chan int{doneA, doneB} {
		select {
		case <-d:
		case <-time.After(10 * time.Second):
			t.Fatal("a session runs on after its VM's pipe gave up")
		}
	}
	eventually(t, "the VM stopped", func() bool { s, _ := vm.snap(); return s })
	end, done := w.shimSession("c", "t3")
	eventually(t, "a new VM", func() bool { return len(h.all()) == 2 })
	end()
	<-done
}

// A VM that fails to start is not kept: the next session tries again.
func TestRoleVMStartFailureIsNotKept(t *testing.T) {
	w, h := streamWorld(t)
	h.mu.Lock()
	h.startErr = errors.New("no KVM right now")
	h.mu.Unlock()
	if code, _, errOut := w.runShim("", nil, w.argv()...); code == 0 || !strings.Contains(errOut, "no KVM") {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	h.mu.Lock()
	h.startErr = nil
	h.mu.Unlock()
	if code, _, _ := w.runShim("x", nil, w.argv()...); code != 5 {
		t.Errorf("after the failure: exit %d", code)
	}
}
