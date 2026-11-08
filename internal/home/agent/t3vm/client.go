package t3vm

// `rubi-home agent-vm --role <id> -- <claude argv...>`: what T3 runs as the role's claude binary. It asks Rubi Home
// for a VM (the folder, T3's arguments and T3's environment go to Home, which filters and checks them) and then
// becomes `container exec` in that VM with T3's stdio, keeping the connection to Home open: when the process ends,
// the connection closes and Home removes the VM. It never runs claude anywhere else.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// ClientOptions are the seams of Run.
type ClientOptions struct {
	// Exec replaces the process (syscall.Exec when nil; Unix only). conn is the open connection to Home, which the new process
	// must inherit; Exec returns only on failure. Where a process cannot be replaced (Windows) Run starts the command
	// instead, with this process's input and output, and returns its exit status (see Relay).
	Exec func(path string, argv, env []string, conn *os.File) error
	// Relay runs the command with this process's stdio and returns its exit status (relayProcess when nil).
	Relay func(path string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	// ExecTool is the name of the program a command from Home may run: the VM backend's tool (agent.ExecToolName when empty;
	// empty on a system with no backend, where no command is run).
	ExecTool string
	// Stdin is what a streamed session relays to the process (os.Stdin when nil).
	Stdin io.Reader
	// Getwd, Environ and LookPath are os.Getwd, os.Environ and exec.LookPath when nil.
	Getwd    func() (string, error)
	Environ  func() []string
	LookPath func(string) (string, error)
}

// ParseArgs reads `--role <id> -- <claude argv...>`.
func ParseArgs(args []string) (role string, claudeArgs []string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--role":
			if i+1 >= len(args) {
				return "", nil, errors.New("--role needs a role id")
			}
			i++
			role = args[i]
		case "--":
			if role == "" {
				return "", nil, errors.New("usage: rubi-home agent-vm --role ID -- CLAUDE-ARGUMENTS...")
			}
			return role, args[i+1:], nil
		default:
			return "", nil, errors.New("usage: rubi-home agent-vm --role ID -- CLAUDE-ARGUMENTS...")
		}
	}
	return "", nil, errors.New("usage: rubi-home agent-vm --role ID -- CLAUDE-ARGUMENTS...")
}

// Run asks Home (at socket) for a VM and replaces this process with `container exec` in it. It returns only when
// that was not possible, with the exit status to use; a plain message is written to stderr. `--version` alone, which T3
// runs to see what it has, is answered by Home with the image's claude version (stdout) and starts no session.
func Run(socket, role string, claudeArgs []string, stdout, stderr io.Writer, o ClientOptions) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(stderr, "rubi-home agent-vm: "+format+"\n", args...)
		return 1
	}
	if o.Getwd == nil {
		o.Getwd = os.Getwd
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	cwd, err := o.Getwd()
	if err != nil {
		return fail("can't tell the working directory: %v", err)
	}
	env := map[string]string{}
	for _, kv := range o.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[k] = v
		}
	}
	conn, err := fsx.DialLocal(socket, 5*time.Second)
	if err != nil {
		return fail("Rubi Home isn't reachable (%v); is it running?", err)
	}
	defer conn.Close() // the copy of the descriptor made below is what stays open
	op := "session.start"
	if len(claudeArgs) == 1 && claudeArgs[0] == "--version" {
		op, env = OpVersion, nil
	}
	b, _ := json.Marshal(StartRequest{Op: op, Role: role, Cwd: cwd, Argv: claudeArgs, Env: env})
	_ = conn.SetDeadline(time.Now().Add(startLimit + time.Minute))
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return fail("Rubi Home didn't take the request: %v", err)
	}
	br := bufio.NewReader(io.LimitReader(conn, maxRequest))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return fail("Rubi Home didn't answer: %v", err)
	}
	var rep StartReply
	if err := json.Unmarshal(line, &rep); err != nil {
		return fail("Rubi Home's answer wasn't understood")
	}
	if !rep.OK {
		msg := rep.Error
		if msg == "" {
			msg = "refused"
		}
		return fail("%s", msg)
	}
	if op == OpVersion {
		if rep.Output == "" {
			return fail("Rubi Home's answer had no version")
		}
		fmt.Fprint(stdout, rep.Output)
		return 0
	}
	_ = conn.SetDeadline(time.Time{})
	if rep.Stream {
		in := o.Stdin
		if in == nil {
			in = os.Stdin
		}
		// Unread bytes of Home's answer (the first chunks may have come with it) are read before the connection.
		return streamClient(struct {
			io.Reader
			io.Writer
		}{io.MultiReader(br, conn), conn}, in, stdout, stderr)
	}
	if len(rep.ExecArgv) < 2 {
		return fail("Rubi Home's answer had no command")
	}
	// What is run is the container tool's `exec`, at the path Home named: a confused or hostile answer must not
	// make this process run something else.
	tool := o.ExecTool
	if tool == "" {
		tool = agent.ExecToolName()
	}
	if tool == "" || filepath.Base(rep.ExecArgv[0]) != tool || rep.ExecArgv[1] != "exec" {
		return fail("Rubi Home's answer was not a container exec")
	}
	path := rep.ExecArgv[0]
	if !filepath.IsAbs(path) {
		if path, err = o.LookPath(path); err != nil {
			return fail("can't find %s: %v", rep.ExecArgv[0], err)
		}
	}
	return handOver(path, rep.ExecArgv, conn, o, stdout, stderr)
}
