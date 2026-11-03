package agent

// What the setup wizard (internal/home/setup) needs from the manager: the resolved projects, a running
// project VM to exec in (with its proxy).

import (
	"context"
	"errors"
	"maps"
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
