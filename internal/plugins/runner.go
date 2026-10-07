package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// Hooks connect running plugins to the core.
type Hooks struct {
	// Handler answers the plugin's requests to Rubi (secrets, approvals, events).
	Handler func(id string) rubiplugin.Handler
	// Launched runs after a plugin process started and initialized, including after a crash restart.
	Launched func(id string)
	// Crashed runs when a plugin keeps crashing and Rubi stops restarting it.
	Crashed func(id string, err error)
	Logf    func(format string, args ...any)
}

var (
	ErrNotRunning = errors.New("the plugin is not running")
	restartDelays = []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 30 * time.Second}
)

const (
	maxCrashes   = 5
	crashWindow  = 10 * time.Minute
	initTimeout  = 10 * time.Second
	stopTimeout  = 2 * time.Second
	pluginAPIEnv = "RUBI_PLUGIN_API"
)

// Runner starts and supervises plugin processes.
type Runner struct {
	Store       *Store
	LogDir      string
	RubiVersion string
	Hooks       Hooks

	mu    sync.Mutex
	procs map[string]*slot
}

type slot struct {
	id, version, tree string
	proc              *proc
	stopped           bool
	crashes           []time.Time
}

type proc struct {
	cmd    *exec.Cmd
	conn   *rubiplugin.Conn
	stdin  io.WriteCloser
	exited chan struct{}
	err    error
}

// Start verifies an installed plugin against its recorded tree hash and launches it.
func (r *Runner) Start(id, version, tree string) error {
	if err := r.Store.Verify(id, version, tree); err != nil {
		return err
	}
	r.mu.Lock()
	if r.procs == nil {
		r.procs = map[string]*slot{}
	}
	if s := r.procs[id]; s != nil && !s.stopped {
		r.mu.Unlock()
		return nil
	}
	s := &slot{id: id, version: version, tree: tree}
	r.procs[id] = s
	r.mu.Unlock()
	return r.launch(s, true)
}

// errBeforeStart marks failures that happened before a process existed.
type errBeforeStart struct{ error }

func (e errBeforeStart) Unwrap() error { return e.error }

// launch starts a process for s. A failed first launch gives up; a failed restart counts as a crash.
func (r *Runner) launch(s *slot, initial bool) (err error) {
	defer func() {
		if err != nil && initial {
			r.mu.Lock()
			s.stopped = true
			if r.procs[s.id] == s {
				delete(r.procs, s.id)
			}
			r.mu.Unlock()
		}
	}()
	m, ok := r.Store.Get(s.id)
	if !ok {
		return fmt.Errorf("plugin %s is not installed", s.id)
	}
	dir := r.Store.Dir(s.id, s.version)
	data := r.Store.DataDir(s.id)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return errBeforeStart{err}
	}
	if err := os.MkdirAll(r.LogDir, 0o700); err != nil {
		return errBeforeStart{err}
	}
	logf, err := os.OpenFile(filepath.Join(r.LogDir, "plugin-"+s.id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return errBeforeStart{err}
	}
	cmd := exec.Command(filepath.Join(dir, filepath.FromSlash(m.Entry)))
	cmd.Dir = data
	cmd.Env = pluginEnv(s.id, data)
	cmd.Stderr = logf
	cmd.SysProcAttr = sysProcAttr()
	stdin, err1 := cmd.StdinPipe()
	stdout, err2 := cmd.StdoutPipe()
	if err := errors.Join(err1, err2); err != nil {
		logf.Close()
		return errBeforeStart{err}
	}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return errBeforeStart{fmt.Errorf("start plugin %s: %w", s.id, err)}
	}
	p := &proc{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	p.conn = rubiplugin.NewConn(stdout, stdin, r.Hooks.Handler(s.id))
	go func() { _ = p.conn.Run(context.Background()) }()
	go func() {
		p.err = cmd.Wait()
		logf.Close()
		close(p.exited)
		r.exited(s, p)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	var res struct {
		API int `json:"api"`
	}
	if err := p.conn.Call(ctx, "initialize", rubiplugin.InitializeParams{API: rubiplugin.API,
		RubiVersion: r.RubiVersion, PluginID: s.id}, &res); err != nil {
		if initial {
			r.mu.Lock()
			s.stopped = true // no restarts for a plugin that never came up
			r.mu.Unlock()
		}
		kill(p) // a restart that fails here is counted as a crash by exited
		return fmt.Errorf("plugin %s didn't initialize: %w", s.id, err)
	}
	r.mu.Lock()
	if s.stopped {
		r.mu.Unlock()
		r.shutdown(p)
		return nil
	}
	s.proc = p
	r.mu.Unlock()
	r.logf("[plugin %s] started %s (pid %d)", s.id, s.version, cmd.Process.Pid)
	if r.Hooks.Launched != nil {
		go r.Hooks.Launched(s.id)
	}
	return nil
}

func pluginEnv(id, home string) []string {
	env := []string{"HOME=" + home, "RUBI_PLUGIN_ID=" + id, pluginAPIEnv + "=" + strconv.Itoa(rubiplugin.API)}
	for _, k := range []string{"PATH", "LANG", "TZ", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// exited handles a process that ended: expected after Stop, a crash otherwise.
func (r *Runner) exited(s *slot, p *proc) {
	r.mu.Lock()
	if s.proc == p {
		s.proc = nil
	}
	if s.stopped || r.procs[s.id] != s {
		r.mu.Unlock()
		return
	}
	now := time.Now()
	var recent []time.Time
	for _, t := range s.crashes {
		if now.Sub(t) < crashWindow {
			recent = append(recent, t)
		}
	}
	s.crashes = append(recent, now)
	n := len(s.crashes)
	giveUp := n >= maxCrashes
	if giveUp {
		s.stopped = true
	}
	r.mu.Unlock()

	r.logf("[plugin %s] exited unexpectedly: %v", s.id, p.err)
	if giveUp {
		if r.Hooks.Crashed != nil {
			r.Hooks.Crashed(s.id, p.err)
		}
		return
	}
	time.AfterFunc(restartDelays[min(n-1, len(restartDelays)-1)], func() {
		r.mu.Lock()
		cur := r.procs[s.id]
		r.mu.Unlock()
		if cur != s || s.stopped {
			return
		}
		if err := r.Store.Verify(s.id, s.version, s.tree); err != nil {
			r.logf("[plugin %s] not restarted: %v", s.id, err)
			return
		}
		if err := r.launch(s, false); err != nil {
			r.logf("[plugin %s] restart failed: %v", s.id, err)
			var before errBeforeStart
			if errors.As(err, &before) {
				r.exited(s, &proc{err: err})
			}
		}
	})
}

// Call sends a request to a running plugin.
func (r *Runner) Call(ctx context.Context, id, method string, params, result any) error {
	r.mu.Lock()
	s := r.procs[id]
	var p *proc
	if s != nil && !s.stopped {
		p = s.proc
	}
	r.mu.Unlock()
	if p == nil {
		return ErrNotRunning
	}
	err := p.conn.Call(ctx, method, params, result)
	if errors.Is(err, rubiplugin.ErrClosed) {
		return errors.New("the plugin stopped while handling the request; try again in a few seconds")
	}
	return err
}

// Running reports whether a plugin process is up.
func (r *Runner) Running(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.procs[id]
	return s != nil && !s.stopped && s.proc != nil
}

// Stop shuts a plugin down: a polite shutdown request, then a kill after a short grace period.
func (r *Runner) Stop(id string) {
	r.mu.Lock()
	s := r.procs[id]
	delete(r.procs, id)
	var p *proc
	if s != nil {
		s.stopped = true
		p, s.proc = s.proc, nil
	}
	r.mu.Unlock()
	if p != nil {
		r.shutdown(p)
	}
}

// StopAll stops every plugin (on lock).
func (r *Runner) StopAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.procs))
	for id := range r.procs {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Stop(id)
		}()
	}
	wg.Wait()
}

func (r *Runner) shutdown(p *proc) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_ = p.conn.Call(ctx, "shutdown", nil, nil)
	_ = p.stdin.Close()
	select {
	case <-p.exited:
	case <-time.After(stopTimeout):
	}
	kill(p)
}

// kill ends the plugin's whole process group (it may have started helpers).
func kill(p *proc) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
	<-p.exited
}

func (r *Runner) logf(format string, args ...any) {
	if r.Hooks.Logf != nil {
		r.Hooks.Logf(format, args...)
	}
}
