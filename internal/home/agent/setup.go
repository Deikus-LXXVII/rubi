package agent

// What the setup wizard (internal/home/setup) needs from the manager: the resolved projects, a running
// project VM to exec in (with its proxy), and the self-test, which holds the slot and answers every question
// of its own project itself.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"
)

// Projects returns the configured projects as the manager resolves them (id and real directory).
func (m *Manager) Projects() []Project { return m.projects() }

// VMRunning says whether the project's VM and runner are up now; it never starts anything.
func (m *Manager) VMRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.projs[id]
	if p == nil || p.runner == nil {
		return false
	}
	select {
	case <-p.runner.Done():
		return false
	default:
		return true
	}
}

// Sandbox is a project's running VM with the environment of its proxy: the way setup runs `claude auth` and
// the isolation probe in it. The environment holds the proxy password; nothing here logs it.
type Sandbox struct {
	Project Project
	vm      VM
	env     map[string]string
}

// Sandbox starts the project's VM, runner and proxy as a session start would, if they aren't running.
func (m *Manager) Sandbox(ctx context.Context, id string) (*Sandbox, error) {
	return m.SandboxRole(ctx, id, "")
}

// SandboxRole is Sandbox for one role's VM of the project ("" or the project's id: the legacy role).
func (m *Manager) SandboxRole(ctx context.Context, id, role string) (*Sandbox, error) {
	p, err := m.projectFor(id, role)
	if err != nil {
		return nil, err
	}
	if _, err := m.ensure(ctx, p); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.runner == nil || p.proxy == nil {
		return nil, errors.New("the project's VM stopped")
	}
	return &Sandbox{Project: p.Project, vm: p.vm, env: maps.Clone(p.proxyEnv)}, nil
}

// Exec runs argv in the VM as the runner's user, with the proxy variables and extra added.
func (s *Sandbox) Exec(ctx context.Context, argv []string, extra map[string]string) (Conn, error) {
	env := maps.Clone(s.env)
	if env == nil {
		env = map[string]string{}
	}
	maps.Copy(env, extra)
	return s.vm.Exec(ctx, argv, env)
}

// SelfTestName names the session the self-test starts.
const SelfTestName = "Rubi self-test"

type selfTest struct {
	project string
	denied  []string // tools asked about, all denied by the manager
	result  chan TurnResult
}

// TurnResult is how the self-test's turn ended.
type TurnResult struct {
	Result  string
	IsError bool
	Closed  bool     // the session closed before a result
	Denied  []string // tools the runner asked about; the manager denied each
}

// event is called with Manager.mu held for every event of the self-test's project.
func (st *selfTest) event(ev Event, result string, isErr bool, status string) {
	var r TurnResult
	switch {
	case ev.Kind == "result" && ev.AgentID == "":
		r = TurnResult{Result: result, IsError: isErr}
	case ev.Kind == "status" && status == "closed":
		r = TurnResult{Closed: true}
	default:
		return
	}
	r.Denied = append([]string(nil), st.denied...)
	select {
	case st.result <- r:
	default:
	}
}

// SelfTest is one project's self-test. While it exists it holds the manager's slot, and the manager denies
// every perm.ask, cred.ask and net.ask of that project by itself: the question is never queued for the plugin,
// so a plugin's decide can neither answer it nor be mistaken for the answer. Its events stay with it too.
type SelfTest struct {
	m    *Manager
	p    *projState
	st   *selfTest
	slot *slotState
	once sync.Once
}

// BeginSelfTest takes a slot of the project for a self-test; it fails while a session of that project runs or
// waits, or another self-test runs. It counts toward the computer-wide limit like a session the user starts.
func (m *Manager) BeginSelfTest(id string) (*SelfTest, error) {
	p, err := m.project(id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	busy := m.self != nil
	for _, s := range m.slots {
		busy = busy || s.project == p.ID
	}
	for _, it := range m.q.list() {
		busy = busy || it.Project == p.ID
	}
	if busy {
		return nil, errors.New("an agent session of this project is running or waiting; try the self-test when it is done")
	}
	st := &selfTest{project: p.ID, result: make(chan TurnResult, 1)}
	slot := &slotState{project: p.ID, role: p.ID, turn: true, last: m.opt.Now()}
	m.self = st
	m.slots = append(m.slots, slot)
	return &SelfTest{m: m, p: p, st: st, slot: slot}, nil
}

// Sandbox is the project's VM for the isolation probe.
func (t *SelfTest) Sandbox(ctx context.Context) (*Sandbox, error) { return t.m.Sandbox(ctx, t.p.ID) }

// Turn runs one session with the prompt (mode default) and waits for its result, at most d. Every tool call
// the runner asks about is denied by the manager; they are listed in the result.
func (t *SelfTest) Turn(ctx context.Context, prompt string, d time.Duration) (TurnResult, error) {
	r, err := t.m.ensure(ctx, t.p)
	if err != nil {
		return TurnResult{}, err
	}
	var res struct {
		ID string `json:"id"`
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	err = r.Call(cctx, "session.start", map[string]any{"prompt": prompt, "name": SelfTestName, "from": "Rubi Home", "mode": "default"}, &res)
	cancel()
	if err != nil {
		return TurnResult{}, fmt.Errorf("starting the session: %w", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = r.Call(cctx, "session.close", map[string]any{"id": res.ID}, nil)
	}()
	select {
	case out := <-t.st.result:
		return out, nil
	case <-time.After(d):
		return TurnResult{}, errors.New("no result in time")
	case <-ctx.Done():
		return TurnResult{}, ctx.Err()
	}
}

// End gives the slot back and lets the plugin's questions through again.
func (t *SelfTest) End() {
	t.once.Do(func() {
		t.m.mu.Lock()
		if t.m.self == t.st {
			t.m.self = nil
		}
		t.m.releaseLocked(t.slot)
		t.m.mu.Unlock()
	})
}
