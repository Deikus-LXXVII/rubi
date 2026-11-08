package home

// The T3 server Rubi Home runs for itself (docs/plans/t3-integration.md sections 8, 12, 13): install the pinned Node
// and T3, start the server on a free port of its own with its own home, pair the bridge with a pairing code minted on
// this computer, write the role instances into T3's settings and serve the session socket. The t3-setup.* ops report
// and drive it for the setup page; the t3-agent.* ops (t3agent.go) use the paired client.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
	"github.com/Deikus-LXXVII/rubi/internal/hostguard"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// t3Server is the part of t3.Server the lifecycle uses (a seam for tests).
type t3Server interface {
	Start(ctx context.Context) (string, error)
	Stop()
	Pid() int
	Alive() bool
	Base() string
	PairingCode(ctx context.Context, scopes []string, ttl time.Duration, label string) (string, error)
}

// Names of the setup steps (t3-setup.run).
const (
	T3StepInstall = "install"
	T3StepStart   = "start"
	T3StepPair    = "pair"
	T3StepStop    = "stop"
	T3StepAll     = "all"
	// T3StepT3 is the setup page's "t3" row: install what is missing (or out of date) and start T3.
	T3StepT3 = "t3"
)

// T3Home is Rubi Home's T3: installation, server, pairing and the paired client.
type T3Home struct {
	h *Helper

	// Seams, set by tests. The zero values are the real thing.
	Fetch FetchFunc
	// Pin replaces the pin embedded in the build (tests).
	Pin *t3Pin
	// Platform is the platform key of the pins (t3Platform when nil).
	Platform func() string
	// NewServer makes the server value for the installed Node and T3 (a *t3.Server when nil).
	NewServer func(node, bin, baseDir, runtimeHome string, port int) t3Server
	// Pair trades a pairing code for a token (t3.Pair when nil).
	Pair func(ctx context.Context, base, code, access string) (t3.Token, error)
	// PickPort chooses a free port on loopback (a random free one when nil).
	PickPort func() (int, error)
	// ServeSessions serves the session socket until ctx ends (Helper.ServeAgentVM when nil).
	ServeSessions func(ctx context.Context) error
	// syncInstances writes T3's role instances (Helper.SyncT3Instances when nil; tests replace it).
	syncInstances func() (t3vm.SyncResult, error)
	// SelfPath is the rubi-home binary the role shims run (os.Executable when nil; a live test builds its own).
	SelfPath func() (string, error)
	// MemAvailable is the computer's free memory in MiB, ok false when unknown (hostguard.MemAvailable when nil).
	MemAvailable func() (mib int, ok bool)
	// VMVersion and VMProbe replace the session server in the self-test (tests).
	VMVersion func(ctx context.Context, role string) (string, error)
	VMProbe   func(ctx context.Context, role string, cmd []string) ([]byte, error)
	// LoginAgents replaces the container the T3 sign-in runs in, and LoginOptions changes the options of its job engine
	// (tests).
	LoginAgents  setup.Agents
	LoginOptions func(*setup.Options)
	// AgentSetup answers the agent-setup.* ops t3-setup.* passes on (Helper.Setup.Handle when nil).
	AgentSetup func(ctx context.Context, op string, args json.RawMessage) (any, error)
	Now        func() time.Time
	// Ceiling is the highest T3 runtime mode the bridge may use (decision 1: full access, the role modes, approvals and
	// the VM still apply on top).
	Ceiling t3.Mode
	// Model is the model a thread starts with unless the call names one.
	Model string

	opMu sync.Mutex // one install/start/pair/stop at a time

	mu          sync.Mutex
	srv         t3Server
	cl          *t3.Client
	runCtx      context.Context // lives while the server does; the watchers and the session server belong to it
	runStop     context.CancelFunc
	sessDone    chan struct{}
	base        context.Context // Home's lifetime (Run); Background until Run is called
	job         *t3Job
	pendingSelf bool // a self-test waiting for the job that was running when sign-in finished
	// onBusyGap runs after that self-test was queued and the lock was released. Tests finish
	// the busy step here, which is the gap that used to drop the self-test.
	onBusyGap func()
	lastErr   *t3Failure
	phase     string
	bytes     atomic.Int64
	wg        sync.WaitGroup // the background step

	loginSetup *setup.Setup
	selfRows   map[string]selfRow // the last self-test, per role

	threads t3Threads
	projMu  sync.Mutex // one project lookup/creation at a time
	watch   map[string]context.CancelFunc
}

type t3Job struct {
	Step    string    `json:"step"`
	Started time.Time `json:"started"`
}

type t3Failure struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

func newT3Home(h *Helper) *T3Home {
	t := &T3Home{h: h, Ceiling: t3.FullAccess, Model: "claude-sonnet-4-6", base: context.Background(), watch: map[string]context.CancelFunc{}}
	t.threads.path = filepath.Join(t.root(), "threads.json")
	return t
}

func (t *T3Home) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *T3Home) platform() string {
	if t.Platform != nil {
		return t.Platform()
	}
	return t3Platform()
}

// root is <helper>/t3: the runtime, T3's home, the role shims, the token and the thread list.
func (t *T3Home) root() string        { return filepath.Join(t.h.Dir, "t3") }
func (t *T3Home) defaultBase() string { return filepath.Join(t.root(), "home") }
func (t *T3Home) tokenFile() string   { return filepath.Join(t.root(), "token.json") }
func (t *T3Home) setPhase(s string)   { t.mu.Lock(); t.phase = s; t.mu.Unlock(); t.bytes.Store(0) }
func (t *T3Home) baseCtx() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base
}

func (t *T3Home) srvAlive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.srv != nil && t.srv.Alive()
}

// Pid is the process id of the T3 server Home runs (0 while it isn't running); the session socket serves only the
// processes below it.
func (t *T3Home) Pid() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil {
		return 0
	}
	return t.srv.Pid()
}

// ---- starting and stopping ----

// Run is what Rubi Home does at its start: when T3 was set up before (installed, and a port chosen), bring it up and
// keep it until ctx ends; then stop the server. It blocks.
func (t *T3Home) Run(ctx context.Context) {
	t.mu.Lock()
	t.base = ctx
	t.mu.Unlock()
	c := t.h.Fresh()
	if _, ok := t.installedNow(); ok && c.T3Port != 0 {
		if err := t.Start(ctx); err != nil {
			log.Printf("T3: %v", err)
		}
	} else if c.T3Port != 0 && c.T3BaseDir != "" {
		// A T3 server that someone else runs, named in the configuration: serve its sessions as before.
		go func() {
			if err := t.h.ServeAgentVM(ctx); err != nil {
				log.Printf("T3 sessions: %v", err)
			}
		}()
	}
	<-ctx.Done()
	t.Wait()
	t.Stop()
}

// Wait waits for the background step started by RunStep (for tests and shutdown).
func (t *T3Home) Wait() { t.wg.Wait() }

func freePort() (int, error) {
	for range 50 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if p != t3.UserPort {
			return p, nil
		}
	}
	return 0, errors.New("no free port found")
}

// portFree says whether a loopback port can be listened on now.
func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// choosePort keeps the port chosen before (the config is the memory) and picks a new one only if that one is taken now
// or was never chosen. It never picks the user's own T3 port. The base folder is recorded with it.
func (t *T3Home) choosePort() (port int, base string, err error) {
	c := t.h.Fresh()
	port, base = c.T3Port, c.T3BaseDir
	if base == "" {
		base = t.defaultBase()
	}
	if port == t3.UserPort || port <= 0 || !portFree(port) {
		pick := t.PickPort
		if pick == nil {
			pick = freePort
		}
		if port, err = pick(); err != nil {
			return 0, "", err
		}
		if port == t3.UserPort {
			return 0, "", fmt.Errorf("port %d is the user's own T3", t3.UserPort)
		}
	}
	if c.T3Port != port || c.T3BaseDir != base {
		if err := t.h.Update(func(c *Config) { c.T3Port, c.T3BaseDir = port, base }); err != nil {
			return 0, "", err
		}
	}
	return port, base, nil
}

func (t *T3Home) newServer(node, bin, base, rtHome string, port int) t3Server {
	if t.NewServer != nil {
		return t.NewServer(node, bin, base, rtHome, port)
	}
	return &t3.Server{Node: node, Bin: bin, BaseDir: base, Port: port, RuntimeHome: rtHome}
}

// Start brings T3 up: the server (a free port of its own, its own home), the bridge's token (paired when there is no
// good one), the role instances and the session socket. It does nothing for what already runs.
func (t *T3Home) Start(ctx context.Context) error {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	return t.startLocked(ctx)
}

func (t *T3Home) startLocked(ctx context.Context) error {
	pin, err := t.pin()
	if err != nil {
		return err
	}
	if _, ok := t.installedNow(); !ok {
		return errors.New("T3 isn't installed (or isn't the pinned version): run the install step of the T3 setup first")
	}
	if !t.srvAlive() && !t.adoptServer(ctx, pin.Version) {
		if err := t.memoryForServer(); err != nil {
			return err
		}
		if err := t.startServer(ctx, pin.Version); err != nil {
			return err
		}
	}
	if t.client() == nil {
		if err := t.pairLocked(ctx, false); err != nil {
			return fmt.Errorf("T3 runs but the pairing failed: %w", err)
		}
	}
	t.startSessions()
	t.resumeWatchers()
	return nil
}

// t3MinFreeMiB is the free memory a T3 server start needs: the pinned server takes about 480 MiB on a Linux server
// (measured on a shared 16 GiB machine with about 3 GiB free, docs/plans/t3-cloud-agents.md slice 0), plus room so
// that starting it doesn't push the computer into its OOM killer (such machines often have no swap).
const t3MinFreeMiB = 800

// memoryForServer refuses a server start when the computer's free memory is known to be too low.
func (t *T3Home) memoryForServer() error {
	avail := hostguard.MemAvailable
	if t.MemAvailable != nil {
		avail = t.MemAvailable
	}
	if mib, ok := avail(); ok && mib < t3MinFreeMiB {
		return fmt.Errorf("only %d MiB of memory are free; T3 needs about %d MiB, so it wasn't started (free some memory and start it again)", mib, t3MinFreeMiB)
	}
	return nil
}

func (t *T3Home) startServer(ctx context.Context, version string) error {
	port, base, err := t.choosePort()
	if err != nil {
		return err
	}
	srv := t.newServer(t.nodeBin(), t.pkgBin(), base, t.runtimeHome(), port)
	t.setPhase("starting T3")
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	got, err := srv.Start(sctx)
	if err != nil {
		srv.Stop()
		return fmt.Errorf("T3 didn't start: %w", err)
	}
	if got != version {
		srv.Stop()
		return fmt.Errorf("T3 reports version %s, the pinned one is %s; it was stopped", clipStr(got, 40), version)
	}
	rc, stop := context.WithCancel(t.baseCtx())
	t.mu.Lock()
	t.srv, t.runCtx, t.runStop = srv, rc, stop
	t.mu.Unlock()
	return nil
}

// adopter is the part of t3.Server that takes over a server an earlier run left.
type adopter interface {
	Adopt(ctx context.Context) (version string, ok bool, err error)
}

// adoptServer takes over the T3 server an earlier run of Rubi Home left running (it crashed or was killed before
// it could stop it), so the agents' turns in it go on. Only a server of the recorded port and base folder, of this
// install and of the pinned version is taken over; one of another version is stopped. It says whether a server
// now runs.
func (t *T3Home) adoptServer(ctx context.Context, version string) bool {
	c := t.h.Fresh()
	if c.T3Port <= 0 || c.T3Port == t3.UserPort {
		return false
	}
	base := c.T3BaseDir
	if base == "" {
		base = t.defaultBase()
	}
	srv := t.newServer(t.nodeBin(), t.pkgBin(), base, t.runtimeHome(), c.T3Port)
	a, ok := srv.(adopter)
	if !ok {
		return false
	}
	got, ok, err := a.Adopt(ctx)
	if err != nil {
		log.Printf("T3: couldn't check for a server an earlier run left: %v", err)
		return false
	}
	if !ok {
		return false
	}
	if got != version {
		srv.Stop()
		log.Printf("T3: stopped the server an earlier run left (version %s; the pinned one is %s)", clipStr(got, 40), version)
		return false
	}
	rc, stop := context.WithCancel(t.baseCtx())
	t.mu.Lock()
	t.srv, t.runCtx, t.runStop = srv, rc, stop
	t.mu.Unlock()
	log.Printf("T3: took over the server an earlier run left (pid %d)", srv.Pid())
	return true
}

// syncProjects is T3's projects for the instance sync (nil while T3 isn't paired or doesn't answer: then no project
// setting is changed).
func (t *T3Home) syncProjects() []t3vm.T3Project {
	cl := t.client()
	if cl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(t.baseCtx(), 15*time.Second)
	defer cancel()
	ps, err := cl.Projects(ctx)
	if err != nil {
		log.Printf("T3 projects (for the role instances): %v", err)
		return nil
	}
	out := make([]t3vm.T3Project, 0, len(ps))
	for _, p := range ps {
		out = append(out, t3vm.T3Project{ID: p.ID, Root: p.WorkspaceRoot})
	}
	return out
}

// startSessions serves the session socket (and writes the role instances) while the server runs.
func (t *T3Home) startSessions() {
	t.mu.Lock()
	if t.sessDone != nil || t.runCtx == nil {
		t.mu.Unlock()
		return
	}
	done := make(chan struct{})
	t.sessDone = done
	ctx := t.runCtx
	t.mu.Unlock()
	// The roles' instances first, so T3 has them when the first thread is started.
	if res, err := t.h.SyncT3Instances(); err != nil {
		log.Printf("T3 role instances: %v", err)
	} else if res.Changed() {
		log.Printf("T3 role instances: added %v, updated %v, removed %v", res.Added, res.Updated, res.Removed)
	}
	serve := t.ServeSessions
	if serve == nil {
		serve = t.h.ServeAgentVM
	}
	go func() {
		defer close(done)
		if err := serve(ctx); err != nil && ctx.Err() == nil {
			log.Printf("T3 sessions: %v", err)
		}
	}()
}

// Stop ends the session socket, the watchers and the server, and forgets the client. It is safe to call twice.
func (t *T3Home) Stop() {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	t.stopLocked()
}

func (t *T3Home) stopLocked() {
	t.mu.Lock()
	stop, srv, cl, done := t.runStop, t.srv, t.cl, t.sessDone
	t.runStop, t.runCtx, t.srv, t.cl, t.sessDone = nil, nil, nil, nil, nil
	t.watch = map[string]context.CancelFunc{}
	t.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	if cl != nil {
		_ = cl.Close()
	}
	if srv != nil {
		srv.Stop()
	}
}

// ---- pairing ----

// tokenGood loads the saved token and says whether it is there and has more than a day left.
func (t *T3Home) tokenGood() (t3.Token, bool) {
	b, err := os.ReadFile(t.tokenFile())
	if err != nil {
		return t3.Token{}, false
	}
	tok, err := t3.LoadToken(b)
	if err != nil || (!tok.Expires.IsZero() && t.now().Add(24*time.Hour).After(tok.Expires)) {
		return tok, false
	}
	return tok, true
}

func (t *T3Home) client() *t3.Client {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.srv == nil || !t.srv.Alive() {
		return nil
	}
	return t.cl
}

// pairLocked makes the client: the saved token if it still works, else a new pairing (force: always a new one). The
// pairing code is minted by T3 on this computer and traded at once; it is never logged or kept.
func (t *T3Home) pairLocked(ctx context.Context, force bool) error {
	t.mu.Lock()
	srv := t.srv
	t.mu.Unlock()
	if srv == nil || !srv.Alive() {
		return errors.New("T3 isn't running")
	}
	access := "full-access"
	if !force {
		if tok, ok := t.tokenGood(); ok {
			cl := t3.New(srv.Base(), tok, t.Ceiling)
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := cl.Projects(pctx)
			cancel()
			if err == nil {
				t.setClient(cl)
				return nil
			}
			_ = cl.Close() // a token T3 doesn't know (its home was reset): pair again
		}
	}
	t.setPhase("pairing")
	code, err := srv.PairingCode(ctx, t3.PairingScopes(access), 10*time.Minute, "Rubi Home")
	if err != nil {
		return err
	}
	pair := t.Pair
	if pair == nil {
		pair = func(ctx context.Context, base, code, access string) (t3.Token, error) {
			return t3.Pair(ctx, &http.Client{Timeout: 30 * time.Second}, base, code, access, "Rubi Home")
		}
	}
	tok, err := pair(ctx, srv.Base(), code, access)
	if err != nil {
		return err
	}
	b, err := tok.Save()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(t.root(), 0o700); err != nil {
		return err
	}
	if err := vault.WriteFileAtomic(t.tokenFile(), b, 0o600); err != nil {
		return err
	}
	t.setClient(t3.New(srv.Base(), tok, t.Ceiling))
	return nil
}

func (t *T3Home) setClient(cl *t3.Client) {
	t.mu.Lock()
	old := t.cl
	t.cl = cl
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// ---- steps and status (t3-setup.*) ----

// RunStep starts a setup step in the background (a download takes minutes) and returns at once; the status says how
// it goes. Only one step runs at a time.
func (t *T3Home) RunStep(step string) error { return t.RunStepFor(step, "", "") }

// RunStepFor is RunStep with the project and role a self-test is for (both empty: every role).
func (t *T3Home) RunStepFor(step, project, role string) error {
	switch step {
	case T3StepInstall, T3StepStart, T3StepPair, T3StepStop, T3StepAll, T3StepT3, T3StepSelfTest:
	default:
		return fmt.Errorf("unknown T3 setup step %q (install, start, pair, stop or all)", clipStr(step, 24))
	}
	t.mu.Lock()
	if t.job != nil {
		j := t.job
		t.mu.Unlock()
		return &setupBusyError{Step: j.Step}
	}
	t.job = &t3Job{Step: step, Started: t.now().UTC()}
	t.lastErr = nil
	ctx := t.base
	t.mu.Unlock()
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.finishJob(step, t.runStep(ctx, step, project, role))
	}()
	return nil
}

// errSetupBusy is the error a setup step returns when another step is already running.
var errSetupBusy = errors.New("T3 setup is busy")

// setupBusyError is errSetupBusy with the step that holds the lock. Callers match it with errors.Is.
type setupBusyError struct{ Step string }

func (e *setupBusyError) Error() string {
	return fmt.Sprintf("T3 setup is busy with the %s step", e.Step)
}

func (e *setupBusyError) Is(target error) bool { return target == errSetupBusy }

// startSelfTestAfterSignIn starts the self-test, or queues it when another T3 step is still
// running. The queue is set under the same lock that sees the busy step, so a job that ends
// in the gap cannot miss it. A queued test runs when that step's job ends.
func (t *T3Home) startSelfTestAfterSignIn() error {
	t.mu.Lock()
	if t.job != nil {
		t.pendingSelf = true
		t.mu.Unlock()
		if t.onBusyGap != nil {
			t.onBusyGap()
		}
		return nil
	}
	t.mu.Unlock()
	err := t.RunStepFor(T3StepSelfTest, "", "")
	if err == nil || !errors.Is(err, errSetupBusy) {
		return err
	}
	// A step started after the check above. Queue it under the lock, and if that step
	// has already finished, start the self-test now rather than leave it queued.
	t.mu.Lock()
	if t.job != nil {
		t.pendingSelf = true
		t.mu.Unlock()
		if t.onBusyGap != nil {
			t.onBusyGap()
		}
		return nil
	}
	t.mu.Unlock()
	return t.startSelfTestAfterSignIn()
}

// finishJob records the outcome of the step that just ended and starts a self-test that was
// waiting for it.
func (t *T3Home) finishJob(step string, err error) {
	t.mu.Lock()
	t.job, t.phase = nil, ""
	if err != nil {
		t.lastErr = &t3Failure{Step: step, Message: err.Error()}
		log.Printf("T3 setup (%s): %v", step, err)
	}
	queued := t.pendingSelf
	t.pendingSelf = false
	t.mu.Unlock()
	if queued {
		if err := t.RunStepFor(T3StepSelfTest, "", ""); err != nil {
			log.Printf("self-test after sign-in: %v", err)
		}
	}
}

func (t *T3Home) runStep(ctx context.Context, step, project, role string) error {
	t.opMu.Lock()
	defer t.opMu.Unlock()
	switch step {
	case T3StepInstall:
		return t.Install(ctx)
	case T3StepStart:
		return t.startLocked(ctx)
	case T3StepPair:
		if !t.srvAlive() {
			return errors.New("T3 isn't running: run the start step first")
		}
		return t.pairLocked(ctx, true)
	case T3StepStop:
		t.stopLocked()
		return nil
	case T3StepSelfTest:
		return t.runSelfTest(ctx, project, role)
	case T3StepAll, T3StepT3:
		if _, ok := t.installedNow(); !ok {
			if err := t.Install(ctx); err != nil {
				return err
			}
		}
		return t.startLocked(ctx)
	}
	return nil
}

// instances lists the role instances in T3's settings.
func (t *T3Home) instances() []string {
	base := t.h.Fresh().T3BaseDir
	if base == "" {
		return []string{}
	}
	b, err := os.ReadFile(filepath.Join(base, "userdata", "settings.json"))
	var s struct {
		ProviderInstances map[string]json.RawMessage `json:"providerInstances"`
	}
	out := []string{}
	if err != nil || json.Unmarshal(b, &s) != nil {
		return out
	}
	for k := range s.ProviderInstances {
		if strings.HasPrefix(k, "claude-") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Status is t3-setup.status: what the setup page shows.
func (t *T3Home) Status() map[string]any {
	pin, _ := t.pin()
	in, installed := t.installedNow()
	c := t.h.Fresh()
	t.mu.Lock()
	running := t.srv != nil && t.srv.Alive()
	paired := running && t.cl != nil
	job, last, phase := t.job, t.lastErr, t.phase
	t.mu.Unlock()
	port := c.T3Port
	// A row is about the steps that can change it: the "t3" row by install, start and t3 itself; the "pair" row by pair.
	mine := func(step string, names ...string) bool {
		for _, n := range names {
			if step == n {
				return true
			}
		}
		return step == T3StepAll
	}
	rowState := func(done bool, names ...string) string {
		switch {
		case job != nil && mine(job.Step, names...):
			return "working"
		case done:
			return "done"
		case last != nil && mine(last.Step, names...):
			return "failed"
		}
		return "todo"
	}
	rowReason := func(ifNot string, names ...string) string {
		if last != nil && mine(last.Step, names...) {
			return last.Message
		}
		return ifNot
	}
	t3Action, t3Reason := "install", ""
	switch {
	case installed && running:
	case installed:
		t3Reason = "T3 is installed but not running"
	default:
		if _, err := os.Stat(t.installedFile()); err == nil {
			t3Action, t3Reason = "upgrade", "The installed T3 is not the pinned version"
		}
	}
	pairReason := ""
	if !running {
		pairReason = "Start T3 first"
	}
	steps := []map[string]any{
		{"id": "t3", "title": setup.TitleT3, "state": rowState(installed && running, T3StepT3, T3StepInstall, T3StepStart),
			"reason": rowReason(t3Reason, T3StepT3, T3StepInstall, T3StepStart), "action": t3Action},
		{"id": T3StepPair, "title": setup.TitlePair, "state": rowState(paired, T3StepPair),
			"reason": rowReason(pairReason, T3StepPair), "action": "pair"},
	}
	out := map[string]any{
		"installed": installed, "version": pin.Version, "node_version": pin.Node.Version, "platform": t.platform(),
		"supported": t3Unavailable(pin, t.platform()) == "", "unsupported_reason": t3Unavailable(pin, t.platform()), "running": running, "paired": paired, "port": port, "instances": t.instances(),
		"steps": steps, "job": nil, "error": nil, "memory_protection": t.h.MemoryProtectionState(),
	}
	if installed {
		out["installed_version"] = in.T3Version
	}
	if running {
		t.mu.Lock()
		if v, ok := t.srv.(interface{ Version() string }); ok {
			out["server_version"] = v.Version()
		}
		t.mu.Unlock()
	}
	if job != nil {
		out["job"] = map[string]any{"step": job.Step, "started": job.Started, "phase": phase, "bytes": t.bytes.Load()}
	}
	if last != nil {
		out["error"] = last
	}
	return out
}

// agentSetupStep are the steps t3-setup.run passes to the agent setup.
var agentSetupStep = map[string]bool{"container": true, "service": true, "network": true, "image": true, "projects": true}

// t3Step says whether a t3-setup.run step is one of T3's own (the others belong to the agent setup).
func t3Step(s string) bool {
	switch s {
	case T3StepInstall, T3StepStart, T3StepPair, T3StepStop, T3StepT3:
		return true
	}
	return false
}

// agentSetup is the agent-setup.* implementation t3-setup.* passes on (the container, its network and image, the
// project folders, the sign-in and the self-test). A seam for tests: the real one probes the whole machine.
func (t *T3Home) agentSetup(ctx context.Context, op string, args json.RawMessage) (any, error) {
	if t.AgentSetup != nil {
		return t.AgentSetup(ctx, op, args)
	}
	return t.h.Setup.Handle(ctx, op, args)
}

// setupStatus is t3-setup.status: the agent setup's status (steps, job, login) with T3's own steps put in after the
// image, and T3's facts (installed, version, running, paired, port, instances) at the top.
func (t *T3Home) setupStatus(ctx context.Context) (any, error) {
	mine := t.Status()
	t3steps, _ := mine["steps"].([]map[string]any)
	out := map[string]any{}
	if res, err := t.agentSetup(ctx, "agent-setup.status", nil); err != nil {
		out["agent_setup_error"] = err.Error()
	} else if b, err := json.Marshal(res); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	steps, _ := out["steps"].([]any)
	at := len(steps)
	for i, s := range steps {
		if m, ok := s.(map[string]any); ok && m["id"] == "image" {
			at = i + 1
		}
	}
	merged := append([]any{}, steps[:at]...)
	for _, s := range t3steps {
		merged = append(merged, s)
	}
	merged = append(merged, steps[at:]...)
	testAt := -1
	for i, s := range merged {
		if m, ok := s.(map[string]any); ok && m["id"] == "selftest" {
			testAt = i
		}
	}
	if testAt >= 0 {
		merged[testAt] = t.selfTestStep()
	} else {
		merged = append(merged, t.selfTestStep())
	}
	// The sign-in of T3 roles is the shared folder's, not the runner path's token: Home reports it itself.
	loginAt := -1
	for i, s := range merged {
		if m, ok := s.(map[string]any); ok && m["id"] == "login" {
			loginAt = i
		}
	}
	merged = placeLogin(merged, loginAt, t.loginStep())
	out["steps"] = merged
	out["login"] = nil
	if li := t.login().LoginProgress(); li.Working && li.URL != "" {
		out["login"] = map[string]any{"url": li.URL, "wants_code": li.WantsCode}
	}
	out["t3_job"], out["t3_error"] = mine["job"], mine["error"]
	delete(mine, "steps")
	delete(mine, "job")
	delete(mine, "error")
	for k, v := range mine {
		out[k] = v
	}
	return out, nil
}

// placeLogin puts the sign-in row where the agent setup had one, or at the end, and says so when
// earlier steps are still to do. A bare "not signed in" with a Sign in button, while the container
// or T3 is not there yet, reads as no status at all.
func placeLogin(merged []any, loginAt int, step map[string]any) []any {
	before := merged
	if loginAt >= 0 && loginAt <= len(merged) {
		before = merged[:loginAt]
	}
	noteLoginWaiting(step, before)
	if loginAt >= 0 && loginAt < len(merged) {
		merged[loginAt] = step
		return merged
	}
	return append(merged, step)
}

// noteLoginWaiting replaces a still-idle sign-in row's reason when an earlier step is not done.
func noteLoginWaiting(step map[string]any, before []any) {
	if st, _ := step["state"].(string); st != "todo" {
		return
	}
	var waiting []string
	for _, raw := range before {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		switch id {
		case "", "projects", "network", "login", "selftest":
			continue
		}
		if s, _ := m["state"].(string); s == "done" {
			continue
		}
		title, _ := m["title"].(string)
		if title == "" {
			title = id
		}
		waiting = append(waiting, title)
	}
	if len(waiting) == 0 {
		return
	}
	reason := "Waiting for earlier steps: " + strings.Join(waiting, ", ") + "."
	step["reason"] = reason
	step["action"] = ""
	rows, _ := step["projects"].([]map[string]any)
	for _, row := range rows {
		switch row["action"] {
		case "login", "login_again", "check":
			row["action"] = ""
			row["state"] = "todo"
			row["reason"] = reason
		}
	}
}

// loginStep is the "login" row: one row per role (the id is the role's), all with the one shared state, because the
// sign-in folder is shared by every role (docs/plans/t3-integration.md decision 4). While a sign-in runs the rows carry
// its link and whether it waits for the code.
func (t *T3Home) loginStep() map[string]any {
	in := agent.SignedInAt(t.signInDir())
	li := t.login().LoginProgress()
	state, reason, action := "todo", "No credential file yet. One sign-in serves every role.", "login"
	if in {
		state, reason, action = "done", "A credential file is present (shared by every role).", "login_again"
	}
	var progress []string
	switch {
	case li.Working:
		state, reason, action, progress = "working", setup.SigningIn, "cancel", li.Progress
		switch {
		case li.CodeSent:
			reason = setup.SigningIn
		case li.URL != "" && li.WantsCode:
			reason = "Paste the code the browser shows."
		case li.URL != "":
			reason = "Open the link and sign in."
		}
	case !in && li.State != "":
		state, reason = li.State, li.Reason
		progress = li.Progress
	}
	if progress == nil {
		progress = []string{}
	}
	rows := []map[string]any{}
	for _, r := range t.h.Agent.T3Projects() {
		row := map[string]any{"id": r.Role, "dir": r.Dir, "state": state, "reason": reason, "action": action}
		if li.Working && li.URL != "" && !li.CodeSent {
			row["url"], row["wants_code"] = li.URL, li.WantsCode
		}
		rows = append(rows, row)
	}
	step := map[string]any{"id": "login", "title": setup.TitleLogin, "state": state, "reason": reason, "action": action,
		"action2": "check", "projects": rows, "progress": progress}
	if li.Working && li.URL != "" && !li.CodeSent {
		step["url"], step["wants_code"] = li.URL, li.WantsCode
	}
	return step
}

// handleSetup serves t3-setup.*: T3's install, start, pair and stop steps, and everything agent-setup.* does.
func (t *T3Home) handleSetup(ctx context.Context, op string, args json.RawMessage) (any, error) {
	var in struct {
		Step    string `json:"step"`
		Method  string `json:"method"`
		Role    string `json:"role"`
		Code    string `json:"code"`
		Project string `json:"project"`
	}
	if len(args) > 0 && string(args) != "null" && json.Unmarshal(args, &in) != nil {
		return nil, errors.New("bad arguments")
	}
	name := strings.TrimPrefix(op, "t3-setup.")
	switch {
	case name == "run" && in.Step == T3StepSelfTest:
		if err := t.checkSelfTarget(in.Project, in.Role); err != nil {
			return nil, err
		}
		if err := t.RunStepFor(T3StepSelfTest, in.Project, in.Role); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case name == "run" && in.Step == "login":
		// Without a method it only checks (the status above looks at the folder).
		if in.Method == "" {
			return map[string]any{"started": true}, nil
		}
		if err := t.startLogin(in.Role, in.Method); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case name == "login.code":
		if err := t.login().LoginCode(in.Code); err != nil {
			return nil, err
		}
		return map[string]any{"sent": true}, nil
	case name == "login.open":
		u, err := t.login().LoginOpen(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": u, "opened": false}, nil
	case name == "cancel":
		stopped := t.login().Cancel()
		res, err := t.agentSetup(ctx, "agent-setup.cancel", args)
		if m, ok := res.(map[string]any); ok && stopped {
			m["stopped"] = true
		}
		return res, err
	case name == "status":
		return t.setupStatus(ctx)
	case name == "run" && t3Step(in.Step):
		if err := t.RunStep(in.Step); err != nil {
			return nil, err
		}
		return map[string]any{"started": true}, nil
	case name == "run" && in.Step == T3StepAll:
		// "Set up everything": T3 as well as the agent path. A busy T3 step is not an error here.
		if err := t.RunStep(T3StepAll); err != nil {
			log.Printf("T3 setup: %v", err)
		}
		return t.agentSetup(ctx, "agent-setup.run", args)
	case name == "run" && !agentSetupStep[in.Step]:
		return nil, fmt.Errorf("unknown setup step %q", clipStr(in.Step, 32))
	case name == "run", name == "project.add", name == "project.resolve", name == "project.remove":
		return t.agentSetup(ctx, "agent-setup."+name, args)
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}
