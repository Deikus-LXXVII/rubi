package agent

// Native T3 borrows role metadata, project permissions and limits from the manager.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// AnthropicHost is the API host a T3 role VM adds to its allow-list (there is no auth gateway in front of it).
const AnthropicHost = "api.anthropic.com"

// T3Role is a role as a T3 session needs it.
type T3Role struct {
	ID, Name, Project string
	// ProjectDir is the role's pinned project directory (the real path): the session's folder must be inside it.
	ProjectDir string
	ClaudeDir  string
	ContentDir string
	Hosts      []string
	// MaxMode is the mode ceiling as a T3 runtime mode: full-access, or auto when agent_allow_bypass is off. A role's
	// default_mode is its default, not a cap (the store fills "default" in for every role), so it does not lower this;
	// the hook gate asks about every call whatever the mode.
	MaxMode string
	// Release unpins the role version the session reads; call it when the session ends.
	Release func()
}

// T3Role finds the role for a session in cwd: it must exist, be active and be made for the project directory as it is
// now (the same check a runner's start makes); the role's version is pinned until Release. Whether cwd lies inside
// ProjectDir is checked against the role and configured project after resolving symlinks.
func (m *Manager) T3Role(id string) (*T3Role, error) {
	if m.roles == nil || m.opt.Dir == "" {
		return nil, errors.New("roles aren't available here")
	}
	if !roles.IsID(id) {
		return nil, errors.New("bad role id")
	}
	r, err := m.roles.Get(id, 0)
	if err != nil {
		return nil, fmt.Errorf("unknown role %q", clip(id, 64))
	}
	if r.Status == "archived" {
		return nil, fmt.Errorf("role %s is archived", clip(r.Name, 40))
	}
	if err := m.roleBoundTo(r, r.Project); err != nil {
		return nil, err
	}
	dir := m.projectDir(r.Project)
	base := filepath.Join(m.opt.Dir, RolesDir, id)
	t := &T3Role{ID: id, Name: r.Name, Project: r.Project, ProjectDir: dir, ClaudeDir: filepath.Join(base, "claude"),
		ContentDir: filepath.Join(base, "content"), Hosts: r.Hosts}
	for _, d := range []string{t.ClaudeDir, t.ContentDir} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("role %s has no directory %s", clip(r.Name, 40), filepath.Base(d))
		}
	}
	if _, err := m.roles.EnforceRevocations(id); err != nil { // withdrawn library skills never reach a starting VM
		log.Printf("library revocations: role %s: %v", clip(id, 64), err)
	}
	top := "full-access"
	if m.opt.AllowBypass != nil && !m.opt.AllowBypass() {
		top = "auto"
	}
	t.MaxMode = top
	t.Release = m.roles.Pin(id, r.Version)
	return t, nil
}

// T3Proxy starts the allow-list proxy of a T3 session on a netpipe listener: the role's allow-list (the base, git and
// forge hosts, the owner's and the role's hosts, what the owner approved for the role) plus the Anthropic API, and
// the same net.ask for the rest. It returns the proxy and the password the VM must present.
func (m *Manager) T3Proxy(r *T3Role, ln net.Listener) (*Proxy, string, error) {
	token, err := NewToken()
	if err != nil {
		return nil, "", err
	}
	p := &projState{Project: Project{ID: r.Project, Dir: r.ProjectDir, Role: r.ID, RoleName: r.Name}}
	hosts := AllowList(m.opt.GitHosts(), append(m.extraHosts(p, r.Hosts), AnthropicHost))
	px, err := StartProxy(ProxyConfig{Listener: ln, AuthToken: token, Hosts: hosts, Exempt: m.opt.GitHosts(),
		Resolve: m.opt.Resolve, Dial: m.opt.Dial, Logf: log.Printf,
		Ask: func(ctx context.Context, host string) bool { return m.askHost(ctx, p, host) }})
	if err != nil {
		return nil, "", fmt.Errorf("proxy: %w", err)
	}
	return px, token, nil
}

// T3Caps are the limits on T3 sessions: per role and on the whole computer (the user's own sessions' limits, the
// per-profile and machine caps of the usage mode; 0 means no cap).
func (m *Manager) T3Caps() (perRole, total int) {
	c := m.usageNow()
	return c.For("").PerProfile, c.Machine()
}

// T3Limits are the caps on running T3 threads for one Bot: per role, per project (all its roles), the Bot's own and
// the computer's. 0 means no cap.
type T3Limits struct{ PerRole, PerProject, PerBot, Total int }

// T3LimitsFor resolves the caps for a Bot ("" is the user's own sessions) from the usage mode and the limits.
func (m *Manager) T3LimitsFor(bot string) T3Limits {
	c := m.usageNow()
	l := c.For(bot)
	return T3Limits{PerRole: l.PerProfile, PerProject: l.PerProject, PerBot: l.Sessions, Total: c.Machine()}
}

// T3ProjectRow is one role of a project as the T3 ops list it.
type T3ProjectRow struct{ Project, Dir, Role, RoleName string }

// T3Projects lists the active isolated roles that are bound to their project directory now (the T3 ops have no
// legacy role: a T3 session always runs in a role's VM).
func (m *Manager) T3Projects() []T3ProjectRow {
	if m.roles == nil || m.opt.Dir == "" {
		return nil
	}
	var out []T3ProjectRow
	for _, row := range m.roles.List("") {
		if row.Legacy || row.Status != "active" {
			continue
		}
		r, err := m.roles.Get(row.ID, 0)
		if err != nil || m.roleBoundTo(r, r.Project) != nil {
			continue
		}
		out = append(out, T3ProjectRow{Project: r.Project, Dir: m.projectDir(r.Project), Role: row.ID, RoleName: row.Name})
	}
	return out
}

// T3ModeSel is a claude permission mode as a T3 thread takes it: T3's runtime mode, and whether the thread plans.
type T3ModeSel struct {
	Claude  string // the mode as the plugin spells it (default, acceptEdits, auto, plan, bypassPermissions)
	Runtime string // approval-required, auto-accept-edits, auto or full-access
	Plan    bool
}

// T3Mode maps a claude-agent mode to T3's. "" is default; dontAsk counts as auto. The bypass modes follow
// agent_allow_bypass like the runner's.
func (m *Manager) T3Mode(s string) (T3ModeSel, error) {
	switch s {
	case "", "default":
		return T3ModeSel{"default", "approval-required", false}, nil
	case "acceptEdits":
		return T3ModeSel{s, "auto-accept-edits", false}, nil
	case "auto":
		return T3ModeSel{s, "auto", false}, nil
	case "plan":
		return T3ModeSel{s, "approval-required", true}, nil
	case "bypassPermissions", "dontAsk":
		if m.opt.AllowBypass != nil && !m.opt.AllowBypass() {
			return T3ModeSel{}, fmt.Errorf("the mode %s is switched off on this computer (agent_allow_bypass)", s)
		}
		if s == "dontAsk" {
			return T3ModeSel{"auto", "auto", false}, nil
		}
		return T3ModeSel{s, "full-access", false}, nil
	}
	return T3ModeSel{}, fmt.Errorf("unknown mode %q", clip(s, 32))
}

// T3ClaudeMode is the claude-agent spelling of a T3 runtime mode (plan is a thread's interaction mode, kept apart).
func T3ClaudeMode(runtime string) string {
	switch runtime {
	case "auto-accept-edits":
		return "acceptEdits"
	case "auto":
		return "auto"
	case "full-access":
		return "bypassPermissions"
	}
	return "default"
}

// OkBot and OkID are the checks the claude-agent ops make on a Bot id and a session id; MaxText is the longest prompt
// or message they take.
func OkBot(s string) bool { return okBot(s) }
func OkID(s string) bool  { return okID(s) }

// MaxText is the longest prompt or message text.
const MaxText = maxQueueText

// SeeBot notes a Bot the first time it is met (its usage mode is then listed), like the claude-agent ops do.
func (m *Manager) SeeBot(bot string) { m.seeBot(bot) }

// Emit hands the plugin an item through claude-agent.next's queue (a T3 thread's turn finishing, for instance).
func (m *Manager) Emit(it Item) {
	m.mu.Lock()
	m.pushLocked(it)
	m.mu.Unlock()
}

// T3RoleRef names a role for a T3 instance.
type T3RoleRef struct {
	ID, Name, ClaudeDir, Project string
	// ProjectDir is the folder the role is pinned to ("" when its project's folder isn't the one it was made for).
	ProjectDir string
}

// T3Roles lists the active isolated roles of every project (the legacy roles have no directories of their own).
func (m *Manager) T3Roles() []T3RoleRef {
	if m.roles == nil || m.opt.Dir == "" {
		return nil
	}
	var out []T3RoleRef
	for _, row := range m.roles.List("") {
		if row.Legacy || row.Status != "active" {
			continue
		}
		ref := T3RoleRef{ID: row.ID, Name: row.Name, ClaudeDir: filepath.Join(m.opt.Dir, RolesDir, row.ID, "claude")}
		if r, err := m.roles.Get(row.ID, 0); err == nil && m.roleBoundTo(r, r.Project) == nil {
			ref.ProjectDir = m.projectDir(r.Project)
			ref.Project = r.Project
		}
		out = append(out, ref)
	}
	return out
}

// ---- the container supervisor ----

// ContainerMount is retained for legacy setup/protocol interfaces; no VM backend is available.
type ContainerMount struct {
	Source, Destination string
	ReadOnly            bool
	// Type is the kind of mount the tool reports: "virtiofs" for a folder or file of the Mac, "tmpfs" for memory.
	Type string
}
