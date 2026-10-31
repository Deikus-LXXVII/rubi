// Package agent is the Mac side of the Agent SDK runner (docs/design/agent-runner.md): the supervisor of a
// per-project VM behind the VM interface, the JSON-RPC client of the runner inside it, the agent slot with
// its disk queue, the allow-list proxy, the git credential helper and the claude-agent.* operations of
// Rubi Home. It never talks to Anthropic itself; only the runner in the VM does, through the proxy.
package agent

import (
	"context"
	"io"
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
}

// StartSpec is what a VM needs to boot a project's runner. Env is handed to the runner through the exec
// channel (never on a command line): RUBI_RUN_TOKEN, HTTPS_PROXY and so on.
type StartSpec struct {
	Project Project
	Env     map[string]string
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
