// Package agent is the Mac side of the Agent SDK runner (docs/design/agent-runner.md): the supervisor of a
// per-project VM behind the VM interface, the JSON-RPC client of the runner inside it, the agent slot with
// its disk queue, the allow-list proxy, the git credential helper and the claude-agent.* operations of
// Rubi Home. It never talks to Anthropic itself; only the runner in the VM does, through the proxy.
package agent

import (
	"context"
	"io"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// State is what a VM is doing.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
)

// Project is a directory the user allowed agents to work in. It is mounted at /work in the VM.
type Project struct {
	ID  string
	Dir string
	// Role, when set, makes this the VM of one isolated role of the project (agent-roles.md section 3): Role is the
	// role's id, RoleName its name (for the container's name), RoleDir the host directory mounted read-only at
	// /rubi-role (agent-roles/<id>/content) and ClaudeDir the one mounted read-write at /rubi-claude
	// (agent-roles/<id>/claude). All empty is the project's legacy role: today's VM, mounts and environment.
	Role      string
	RoleName  string
	RoleDir   string
	ClaudeDir string
	// Shield protects the project from a role VM (set by Home for each start): EmptyDir is a host directory that is
	// mounted read-only over /work/.rubi (the legacy role's state), and ReadOnly names directories of the project
	// (".claude") that are mounted read-only over their place in /work. A single file can't be bound, so
	// /work/CLAUDE.md stays writable (docs/design/agent-roles.md section 3).
	EmptyDir string
	ReadOnly []ShieldMount
	// snapFailed names the ReadOnly places whose copy came out empty (set by roleShield; Home tells the plugin).
	snapFailed []string
}

// ShieldMount is one read-only overlay: the project-relative place and the Home-owned host directory shown there.
type ShieldMount struct {
	Name   string
	Source string
}

// RoleMountDir and RoleClaudeDir are where a role VM sees its content and its config directory.
const (
	RoleMountDir  = "/rubi-role"
	RoleClaudeDir = "/rubi-claude"
)

// StartSpec is what a VM needs to boot a project's runner. Env is handed to the runner through the exec
// channel (never on a command line): RUBI_RUN_TOKEN, HTTPS_PROXY and so on.
type StartSpec struct {
	Project Project
	Env     map[string]string
	// Prepare, when set, is called once the VM is up and before the runner starts, with the Mac's address
	// on the VM's network ("" when the VM has none to report). The variables it returns are added to Env.
	// An error stops the VM.
	Prepare func(ctx context.Context, gateway string) (map[string]string, error)
	// Pipe is what a VM without a network (NetlessVM) reaches: the listeners its streams are offered to. The
	// proxy and the auth gateway already serve them, so nothing on the Mac listens for the VM.
	Pipe *PipeSpec
}

// PipeSpec names the services of a netless VM's pipe (netpipe): a stream to a nil one is reset.
type PipeSpec struct {
	Proxy   *netpipe.Listener
	Gateway *netpipe.Listener
}

// The runner's view of its two services: the forwarder (runner/container/rubi-netpipe) listens on these in the VM.
const (
	PipeProxyAddr   = "127.0.0.1:3128"
	PipeGatewayAddr = "127.0.0.1:3129"
)

// NetlessVM is implemented by a VM that has no network at all (`container run --network none`). Its
// only way out is the Pipe of the StartSpec; Start gets no gateway address and does not call Prepare.
type NetlessVM interface {
	VM
	Netless() bool
}

// prepareEnv runs spec.Prepare and returns the runner's environment.
func prepareEnv(ctx context.Context, spec StartSpec, gateway string) (map[string]string, error) {
	if spec.Prepare == nil {
		return spec.Env, nil
	}
	extra, err := spec.Prepare(ctx, gateway)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(spec.Env)+len(extra))
	for k, v := range spec.Env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	return env, nil
}

// Conn is a process in the VM seen from the Mac: stdin, stdout, and an end.
type Conn interface {
	io.Reader // the process's stdout
	io.Writer // the process's stdin
	// CloseWrite closes the process's stdin.
	CloseWrite() error
	// Close ends the process (and releases what the supervisor kept for it).
	Close() error
	// Done is closed when the process has ended.
	Done() <-chan struct{}
}

// ReadyNotifier is implemented by a Conn that keeps something only until the runner is up (the
// environment file of an exec); the client calls Ready after a successful handshake.
type ReadyNotifier interface{ Ready() }

// VM is one project's virtual machine.
type VM interface {
	// Start boots the VM if needed and starts the runner in it, returning the runner's stdio.
	Start(ctx context.Context, spec StartSpec) (Conn, error)
	// Exec runs another process in the running VM (git credential helper tests, diagnostics).
	Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error)
	// Stop ends the VM and everything in it.
	Stop(ctx context.Context) error
	State() State
}
