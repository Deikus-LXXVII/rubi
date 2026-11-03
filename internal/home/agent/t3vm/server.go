package t3vm

// The session server: Rubi Home's end of `rubi-home agent-vm`. T3 starts one claude per provider session through the
// role's shim; the shim sends the folder, T3's argument list and T3's environment here, and Home decides everything:
// whether the role may work in that folder, which arguments are run, what the VM is made of, what it may reach. The
// answer names the container and the environment file; the shim runs claude in it. The VM lives as long as the
// connection (docs/plans/t3-integration.md section 12).

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// threadOf is the T3 thread the session serves, from T3's environment: T3CODE_THREAD_ID (what the pinned T3 sets, checked
// live) or T3_AGENT_THREAD.
func threadOf(env map[string]string) string {
	if v := env["T3CODE_THREAD_ID"]; v != "" {
		return v
	}
	return env["T3_AGENT_THREAD"]
}

var threadRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// SocketName is the session server's socket in Home's directory.
const SocketName = "agent-vm.sock"

// SocketPath is the socket's path in dir.
func SocketPath(dir string) string { return filepath.Join(dir, SocketName) }

// DefaultClaude is claude's place in the runner image (the Agent SDK's arm64 build, which the image carries).
const DefaultClaude = "/opt/rubi-runner/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64/claude"

// StartRequest is the one line a shim sends.
type StartRequest struct {
	Op   string            `json:"op"` // "session.start"
	Role string            `json:"role"`
	Cwd  string            `json:"cwd"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// OpVersion asks for the claude version of the image: T3 runs `claude --version` through the binary it is given to
// find out what it has, with no session behind it.
const OpVersion = "claude.version"

// StartReply is Home's one-line answer.
type StartReply struct {
	OK        bool     `json:"ok"`
	Error     string   `json:"error,omitempty"`
	Output    string   `json:"output,omitempty"` // OpVersion: what `claude --version` printed
	Container string   `json:"container,omitempty"`
	EnvFile   string   `json:"env_file,omitempty"`
	ExecArgv  []string `json:"exec_argv,omitempty"`
}

const (
	maxRequest   = 1 << 20
	maxAfterJunk = 64 << 10 // what a connected shim may send after its request before it is dropped
	readLimit    = 15 * time.Second
	startLimit   = 12 * time.Minute // the image may have to be built
	stopLimit    = 30 * time.Second
	maxPending   = 8 // connections whose request is still being read or acted on
)

// Backend is the manager's part (agent.Manager).
type Backend interface {
	Asker
	T3Role(id string) (*agent.T3Role, error)
	T3Proxy(r *agent.T3Role, ln net.Listener) (*agent.Proxy, string, error)
	T3Caps() (perRole, total int)
}

// Host is the container supervisor's part (agent.AppleContainer).
type Host interface {
	EnsureImage(ctx context.Context) error
	ImageTag() string
	ContainerBin() string
	RunDetached(ctx context.Context, argv []string) error
	RunOutput(ctx context.Context, argv []string) ([]byte, error)
	// ListContainers names every container, and InspectMounts says what a container has mounted (the sweep at
	// Home's start, and the check after a VM is run).
	ListContainers(ctx context.Context) ([]string, error)
	InspectMounts(ctx context.Context, name string) ([]agent.ContainerMount, error)
	StartSessionPipe(ctx context.Context, name string, spec *agent.PipeSpec, env map[string]string, giveUp func()) (stop func(), err error)
	RemoveContainer(ctx context.Context, name string) error
}

// Options configure a Server.
type Options struct {
	// Dir is Home's directory: the files of the VMs are kept under Dir/t3vm, the sign-in in Dir/agent-signin.
	Dir     string
	Backend Backend
	Host    Host
	// T3Port is the port of Home's T3 server; the VM's forwarder listens on the same port, so the MCP address T3 hands
	// to claude works inside the VM. Attachments is T3's attachments folder (--add-dir names it).
	T3Port      int
	Attachments string
	// Claude is claude's path in the image (DefaultClaude when empty).
	Claude string
	// DialT3 reaches T3's server (127.0.0.1:T3Port when nil).
	DialT3 func(ctx context.Context) (net.Conn, error)
	// PeerOK says whether a connection comes from the owner (the same user id as Home when nil).
	PeerOK func(net.Conn) bool
	// T3Pid is the process id of the T3 server when Home runs it (0 or nil when it doesn't: the server is the user's
	// own or runs elsewhere). When it is known, a connection must also come from a descendant of that process; the
	// shim is started by T3, so anything else on the socket (another program of the same user) is refused. Without
	// it the check is the user id only, which is all that can be said about a T3 Home did not start.
	T3Pid func() int
	Logf  func(format string, args ...any)

	// Hooks for tests, set only from this package's tests: testEnv is added to every session's environment (to point
	// claude at a scripted API) and testAPI serves the VM's 127.0.0.1:3129 (the gateway port of the runner VMs,
	// unused here) from a dial. Home never sets them.
	testEnv map[string]string
	testAPI func(ctx context.Context) (net.Conn, error)
}

// Server serves the session socket.
type Server struct {
	opt Options

	mu       sync.Mutex
	live     int
	perRole  map[string]int
	versions map[string]string
	verCalls map[string]*verCall // claude --version asked of the image right now
	verFail  map[string]verNeg   // and how it last failed
	base     context.Context     // ends when Serve does
	lock     *os.File            // held while Serve runs: one Home serves sessions on a helper directory
	pending  int
	sessions map[*session]struct{}
	wg       sync.WaitGroup
}

// NewServer makes a Server.
func NewServer(opt Options) *Server {
	if opt.Claude == "" {
		opt.Claude = DefaultClaude
	}
	if opt.Logf == nil {
		opt.Logf = log.Printf
	}
	if opt.DialT3 == nil {
		addr := fmt.Sprintf("127.0.0.1:%d", opt.T3Port)
		opt.DialT3 = func(ctx context.Context) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
		}
	}
	if opt.PeerOK == nil {
		opt.PeerOK = func(c net.Conn) bool {
			uid, err := peerUID(c)
			if err != nil || int(uid) != os.Getuid() {
				return false
			}
			if opt.T3Pid != nil {
				if t3 := opt.T3Pid(); t3 > 1 {
					return peerDescendsFrom(c, t3)
				}
			}
			return true
		}
	}
	return &Server{opt: opt, perRole: map[string]int{}, versions: map[string]string{}, verCalls: map[string]*verCall{}, verFail: map[string]verNeg{}, sessions: map[*session]struct{}{}}
}

// Sessions is the number of sessions with a VM.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// Serve listens on path (mode 0600) until ctx ends, then ends every session and returns.
func (s *Server) Serve(ctx context.Context, path string) error {
	if err := s.lockAndSweep(ctx); err != nil {
		return err
	}
	defer s.unlock()
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and isn't a socket", path)
		}
		_ = os.Remove(path) // left over from an earlier run
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	stopped := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	defer os.Remove(path)
	defer s.wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if stopped.Err() != nil {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.ServeConn(ctx, conn)
		}()
	}
}

func reply(conn net.Conn, r StartReply) {
	_ = conn.SetWriteDeadline(time.Now().Add(readLimit))
	b, _ := json.Marshal(r)
	_, _ = conn.Write(append(b, '\n'))
}

func refuse(conn net.Conn, why string) { reply(conn, StartReply{Error: why}) }

// ServeConn handles one shim from its request to the end of its session. It closes conn.
func (s *Server) ServeConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if !s.opt.PeerOK(conn) {
		refuse(conn, "Rubi Home only serves its own user (and, when it runs T3, the processes below it)")
		return
	}
	s.mu.Lock()
	if s.pending >= maxPending {
		s.mu.Unlock()
		refuse(conn, "Rubi Home is busy starting other sessions; try again")
		return
	}
	s.pending++
	s.mu.Unlock()
	pending := true
	endPending := func() {
		if pending {
			pending = false
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
		}
	}
	defer endPending()

	_ = conn.SetReadDeadline(time.Now().Add(readLimit))
	br := bufio.NewReaderSize(conn, maxRequest+2)
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > maxRequest {
		refuse(conn, "Rubi Home could not read the request")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req StartRequest
	if err := json.Unmarshal(bytes.TrimSpace(line), &req); err != nil || (req.Op != "session.start" && req.Op != OpVersion) {
		refuse(conn, "Rubi Home does not understand the request")
		return
	}
	// From here the connection is watched: a shim that goes away (or sends more than it should) ends the session,
	// also while the VM is still starting, and a version probe whose shim went away is not waited for.
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		defer cancel()
		if n, _ := io.Copy(io.Discard, io.LimitReader(br, maxAfterJunk)); n >= maxAfterJunk {
			s.opt.Logf("t3vm: a shim sent too much after its request; ending its session")
		}
	}()

	if req.Op == OpVersion {
		vctx, vcancel := context.WithTimeout(sctx, 2*time.Minute)
		defer vcancel()
		out, err := s.version(vctx, req)
		if err != nil {
			refuse(conn, err.Error())
			return
		}
		reply(conn, StartReply{OK: true, Output: out})
		return
	}

	ses, rep, err := s.start(sctx, cancel, req)
	if err != nil {
		s.opt.Logf("t3vm: session for role %q refused: %v", clipStr(req.Role, 64), err)
		refuse(conn, err.Error())
		return
	}
	s.mu.Lock()
	s.sessions[ses] = struct{}{}
	s.mu.Unlock()
	endPending()
	defer func() {
		ses.teardown()
		s.mu.Lock()
		delete(s.sessions, ses)
		s.mu.Unlock()
	}()
	reply(conn, rep)
	select {
	case <-gone:
	case <-sctx.Done():
	}
}

// versionRetry is how long a failed version probe is remembered (T3 asks often; a broken image would otherwise be
// started over and over).
var versionRetry = 30 * time.Second

type verCall struct {
	done chan struct{}
	out  string
	err  error
}

type verNeg struct {
	at  time.Time
	err error
}

// version is claude's version in the image, asked of a throwaway VM with no mounts and no network. One probe runs at
// a time for an image, its answer is kept, and a failure is kept for a short while. A caller whose connection goes
// away stops waiting; the probe itself runs on for the others and ends with its own timeout.
func (s *Server) version(ctx context.Context, req StartRequest) (string, error) {
	if len(req.Argv) != 1 || req.Argv[0] != "--version" {
		return "", errors.New("Rubi Home only answers --version here")
	}
	role, err := s.opt.Backend.T3Role(req.Role) // the role must exist; nothing of it is used
	if err != nil {
		return "", err
	}
	role.Release()
	image := s.opt.Host.ImageTag()
	s.mu.Lock()
	if v, ok := s.versions[image]; ok {
		s.mu.Unlock()
		return v + "\n", nil
	}
	if n, ok := s.verFail[image]; ok && time.Since(n.at) < versionRetry {
		s.mu.Unlock()
		return "", n.err
	}
	call, running := s.verCalls[image]
	if !running {
		call = &verCall{done: make(chan struct{})}
		s.verCalls[image] = call
		go func() {
			pctx, cancel := context.WithTimeout(s.baseCtx(), 2*time.Minute)
			defer cancel()
			out, err := s.probeVersion(pctx, image)
			s.mu.Lock()
			delete(s.verCalls, image)
			if err != nil {
				s.verFail[image] = verNeg{time.Now(), err}
			} else {
				delete(s.verFail, image)
				s.versions[image] = out
			}
			s.mu.Unlock()
			call.out, call.err = out, err
			close(call.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-call.done:
		if call.err != nil {
			return "", call.err
		}
		return call.out + "\n", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// baseCtx is the context that ends when Serve does.
func (s *Server) baseCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base == nil {
		return context.Background()
	}
	return s.base
}

func (s *Server) probeVersion(ctx context.Context, image string) (string, error) {
	if err := s.opt.Host.EnsureImage(ctx); err != nil {
		return "", fmt.Errorf("the VM image: %w", err)
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Named, so that the sweep at Home's start removes one that a crash left.
	name := ContainerPrefix + "ver-" + hex.EncodeToString(b)
	bin, err := s.containerBin()
	if err != nil {
		return "", err
	}
	out, err := s.opt.Host.RunOutput(ctx, []string{bin, "run", "--rm", "--name", name, "--network", "none", "--user", uid,
		"--cap-drop", "ALL", "--read-only", "--entrypoint", s.opt.Claude, image, "--version"})
	if err != nil {
		return "", fmt.Errorf("asking the image for claude's version: %w", err)
	}
	v := strings.TrimSpace(string(out))
	if i := strings.LastIndex(v, "\n"); i >= 0 { // the tool may print progress lines before the answer
		v = strings.TrimSpace(v[i+1:])
	}
	if v == "" || len(v) > 200 {
		return "", errors.New("claude's version was not readable")
	}
	return v, nil
}

// containerBin is the container tool as an absolute path: the shim runs it with `exec`, so a name in the PATH of a
// process Home does not control must not be what it finds.
func (s *Server) containerBin() (string, error) {
	bin := s.opt.Host.ContainerBin()
	if filepath.IsAbs(bin) {
		return bin, nil
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("the container tool isn't installed: %w", err)
	}
	return filepath.Abs(p)
}

func clipStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// session is one running VM.
type session struct {
	s      *Server
	role   string
	giveUp func()

	slot  func() // frees the place under the caps
	once  sync.Once
	steps []func()
}

// onEnd registers what teardown does, last registered first.
func (x *session) onEnd(f func()) { x.steps = append(x.steps, f) }

// teardown ends the session. Its place under the caps is freed first: once the connection is gone the session is
// over for the shim, and the next start must not wait for the container tool to remove the VM.
func (x *session) teardown() {
	x.once.Do(func() {
		if x.slot != nil {
			x.slot()
		}
		for i := len(x.steps) - 1; i >= 0; i-- {
			x.steps[i]()
		}
		x.steps = nil
	})
}

// admitRetry is how long a start waits for a place under the caps before it is refused (a session that just ended
// frees its place a moment later). A variable so that tests can shorten it.
var admitRetry = 10 * time.Second

// admitWait is admit, retried until the wait is over. The caps count T3 sessions only: the runner VMs' slots
// (Manager.slots) are a separate count, so the two kinds of session do not share the computer-wide limit.
func (s *Server) admitWait(ctx context.Context, role string) (func(), error) {
	deadline := time.Now().Add(admitRetry)
	for {
		release, err := s.admit(role)
		if err == nil {
			return release, nil
		}
		if !time.Now().Before(deadline) {
			return nil, err
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// admit counts a session against the caps (per role and on the computer).
func (s *Server) admit(role string) (release func(), err error) {
	perRole, total := s.opt.Backend.T3Caps()
	s.mu.Lock()
	defer s.mu.Unlock()
	if total > 0 && s.live >= total {
		return nil, errors.New("the computer's session limit is reached; end one and try again")
	}
	if perRole > 0 && s.perRole[role] >= perRole {
		return nil, fmt.Errorf("this role already runs %d session(s), its limit; end one and try again", perRole)
	}
	s.live++
	s.perRole[role]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.live--
			if s.perRole[role]--; s.perRole[role] <= 0 {
				delete(s.perRole, role)
			}
			s.mu.Unlock()
		})
	}, nil
}

// start makes the VM. On an error everything it made is gone.
func (s *Server) start(ctx context.Context, end func(), req StartRequest) (_ *session, rep StartReply, err error) {
	o := s.opt
	role, err := o.Backend.T3Role(req.Role)
	if err != nil {
		return nil, rep, err
	}
	ses := &session{s: s, role: role.ID, giveUp: end}
	defer func() {
		if err != nil {
			ses.teardown()
		}
	}()
	ses.onEnd(role.Release)

	// The arguments first: nothing is made for a command line Home would not run.
	policy := Policy{Ceiling: t3.Mode(role.MaxMode), Cwd: req.Cwd, Attachments: o.Attachments, T3Port: o.T3Port}
	argv, err := ClampArgv(req.Argv, policy)
	if err != nil {
		return nil, rep, err
	}
	release, err := s.admitWait(ctx, role.ID)
	if err != nil {
		return nil, rep, err
	}
	ses.slot = release

	name, err := NewName(role.ID)
	if err != nil {
		return nil, rep, err
	}
	bin, err := s.containerBin()
	if err != nil {
		return nil, rep, err
	}
	// The image first, so that nothing between resolving the paths and running the container takes minutes (a
	// long gap is a long window for a path to be swapped).
	sctx, cancel := context.WithTimeout(ctx, startLimit)
	defer cancel()
	if err := o.Host.EnsureImage(sctx); err != nil {
		return nil, rep, fmt.Errorf("the VM image: %w", err)
	}
	files, err := s.writeFiles()
	if err != nil {
		return nil, rep, err
	}
	spec := SessionSpec{
		Bin: bin, Image: o.Host.ImageTag(), Name: name, Role: role.ID,
		ClaudeDir: role.ClaudeDir, ContentDir: role.ContentDir, Cwd: req.Cwd, TrustedRoot: role.ProjectDir,
		HelperDir: o.Dir, SignInDir: files.signIn, ReadOnly: ExtraDirs(argv, req.Cwd),
		HookScript: files.hook, ManagedSettings: files.settings, T3Port: o.T3Port,
	}
	runArgs, want, err := spec.Plan() // checks the folder, the mounts and the port
	if err != nil {
		return nil, rep, err
	}
	// From the moment the container is asked for, a failure removes it.
	ses.onEnd(func() {
		rctx, c := context.WithTimeout(context.Background(), stopLimit)
		defer c()
		if err := o.Host.RemoveContainer(rctx, name); err != nil {
			s.opt.Logf("t3vm: removing %s: %v", name, err)
		}
	})
	if err := o.Host.RunDetached(sctx, runArgs); err != nil {
		return nil, rep, fmt.Errorf("starting the VM: %w", err)
	}
	if err := s.checkMounts(sctx, name, want); err != nil {
		return nil, rep, err
	}

	proxyL, hookL, t3L := netpipe.NewListener("proxy", 16), netpipe.NewListener("hook", 16), netpipe.NewListener("t3", 16)
	px, proxyToken, err := o.Backend.T3Proxy(role, proxyL)
	if err != nil {
		return nil, rep, err
	}
	ses.onEnd(func() { px.Close() })
	hook := &http.Server{Handler: s.gate(role, req.Cwd, threadOf(req.Env)), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = hook.Serve(hookL) }()
	ses.onEnd(func() { hook.Close(); hookL.Close() })
	fwd := &forwarder{dial: o.DialT3}
	fwd.wg.Add(1) // the accept loop; the connections it carries are added by it while it runs
	go fwd.serve(t3L)
	ses.onEnd(func() { t3L.Close(); fwd.close() })

	pipe := &agent.PipeSpec{Proxy: proxyL, Hook: hookL, T3: t3L}
	if o.testAPI != nil {
		apiL := netpipe.NewListener("api", 16)
		api := &forwarder{dial: o.testAPI}
		api.wg.Add(1)
		go api.serve(apiL)
		ses.onEnd(func() { apiL.Close(); api.close() })
		pipe.Gateway = apiL
	}
	stopPipe, err := o.Host.StartSessionPipe(sctx, name, pipe, spec.PipeEnv(),
		func() {
			s.opt.Logf("t3vm: the network pipe of %s keeps failing; ending the session", name)
			ses.giveUpNow()
		})
	if err != nil {
		return nil, rep, fmt.Errorf("the network pipe: %w", err)
	}
	ses.onEnd(stopPipe)

	body, err := spec.EnvLines(req.Env, map[string]string{"HTTPS_PROXY": "http://rubi:" + proxyToken + "@" + agent.PipeProxyAddr, "NO_PROXY": "127.0.0.1"})
	if err != nil {
		return nil, rep, err
	}
	if len(o.testEnv) > 0 {
		extra, err := agent.EnvFile(o.testEnv)
		if err != nil {
			return nil, rep, err
		}
		body = append(body, extra...)
	}
	envFile := filepath.Join(files.envDir, name+".env")
	if err := os.WriteFile(envFile, body, 0o600); err != nil {
		return nil, rep, err
	}
	ses.onEnd(func() { _ = os.Remove(envFile) })
	exec, err := spec.ExecArgs(envFile, o.Claude, argv)
	if err != nil {
		return nil, rep, err
	}
	return ses, StartReply{OK: true, Container: name, EnvFile: envFile, ExecArgv: exec}, nil
}

func (x *session) giveUpNow() { x.giveUp() }

func (s *Server) gate(role *agent.T3Role, cwd, thread string) http.Handler {
	if !threadRE.MatchString(thread) {
		thread = "" // not an id T3 would make: asks keep claude's own session id
	}
	g := NewGate(s.opt.Backend, Target{Project: role.Project, Role: role.ID, Cwd: cwd, Thread: thread})
	g.Logf = s.opt.Logf
	return g
}

// checkMounts compares what the container tool says the new VM has mounted with what was asked for, and checks that
// no source has turned into a link since the paths were resolved: a path swapped between the check and the run would
// have been followed by the tool, and the VM would hold something else than Home vouched for.
func (s *Server) checkMounts(ctx context.Context, name string, want []ExpectedMount) error {
	got, err := s.opt.Host.InspectMounts(ctx, name)
	if err != nil {
		return fmt.Errorf("checking the VM's mounts: %w", err)
	}
	have := map[string]agent.ContainerMount{}
	for _, m := range got {
		have[m.Destination] = m
	}
	// the one in-memory mount RunArgs asks for (--tmpfs /tmp) is listed too
	if m, ok := have["/tmp"]; ok && m.Type == "tmpfs" {
		delete(have, "/tmp")
	}
	for _, w := range want {
		m, ok := have[w.Destination]
		if !ok || m.Source != w.Source || m.ReadOnly != w.ReadOnly || m.Type != "virtiofs" {
			return fmt.Errorf("the VM does not have the mounts Home asked for (%s); it was removed", w.Destination)
		}
		if real, err := filepath.EvalSymlinks(w.Source); err != nil || real != w.Source {
			return fmt.Errorf("%s changed while the VM started; it was removed", w.Destination)
		}
		delete(have, w.Destination)
	}
	if len(have) > 0 {
		return errors.New("the VM has mounts Home did not ask for; it was removed")
	}
	return nil
}

type sessionFiles struct{ hook, settings, signIn, envDir string }

// writeFiles puts the hook script and the managed settings in place (read-only to the VM, so 0644 files in a
// directory only Home writes) and makes the sign-in folder and the environment folder.
func (s *Server) writeFiles() (sessionFiles, error) {
	dir := filepath.Join(s.opt.Dir, "t3vm")
	f := sessionFiles{hook: filepath.Join(dir, "rubi-hook.mjs"), settings: filepath.Join(dir, "managed-settings.json"),
		signIn: filepath.Join(s.opt.Dir, "agent-signin"), envDir: filepath.Join(dir, "env")}
	if err := os.MkdirAll(f.envDir, 0o700); err != nil {
		return f, err
	}
	// an env directory made by an earlier build, or by hand, may be wider (it holds the proxy's password)
	if fi, err := os.Lstat(f.envDir); err != nil || !fi.IsDir() {
		return f, errors.New("t3vm: the environment directory isn't a directory")
	}
	if err := os.Chmod(f.envDir, 0o700); err != nil {
		return f, err
	}
	if err := os.MkdirAll(f.signIn, 0o700); err != nil {
		return f, err
	}
	realSignIn, err := filepath.EvalSymlinks(f.signIn)
	if err != nil {
		return f, err
	}
	settings, err := ManagedSettings(realSignIn)
	if err != nil {
		return f, err
	}
	for path, body := range map[string][]byte{f.hook: HookScript, f.settings: settings} {
		if err := putFile(path, body, 0o644); err != nil {
			return f, err
		}
	}
	return f, nil
}

// putFile makes path hold body (mode perm), replacing it atomically and not at all when it already does: a running
// VM has the file bound in.
func putFile(path string, body []byte, perm os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, body) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == perm {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// forwarder carries the VM's connections to T3's server.
type forwarder struct {
	dial func(ctx context.Context) (net.Conn, error)
	wg   sync.WaitGroup

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// track remembers a connection so that close can end it; false once the forwarder is closed.
func (f *forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	if f.conns == nil {
		f.conns = map[net.Conn]struct{}{}
	}
	f.conns[c] = struct{}{}
	return true
}

// close ends every connection being carried and waits for the loops to finish.
func (f *forwarder) close() {
	f.mu.Lock()
	f.closed = true
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *forwarder) serve(ln net.Listener) {
	defer f.wg.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			up, err := f.dial(ctx)
			cancel()
			if err != nil {
				return
			}
			defer up.Close()
			if !f.track(c) || !f.track(up) {
				return
			}
			splice(c, up)
		}()
	}
}

type closeWriter interface{ CloseWrite() error }

// splice copies both ways until both directions have ended, passing a half close on.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
