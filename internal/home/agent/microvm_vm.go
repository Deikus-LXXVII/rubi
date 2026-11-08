//go:build unix

package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/guest"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

// MicroVM is a project's VM on the Linux backend (docs/design/linux-microvm.md): QEMU/KVM started by microvm.Launcher,
// the shares served by virtiofsd confined to them, and the guest agent (package guest) on the control channel, through
// which Home mounts the shares, runs the network pipe and the runner, and ends the VM. It has no network card at all.
type MicroVM struct {
	// Launcher starts the VM's processes (the pinned virtiofsd, its libraries and Home's confining helper).
	Launcher microvm.Launcher
	// QEMU, Firmware, Kernel and Rootfs are the pinned files (Firmware: qboot on x86, empty on arm64); Arch is the
	// computer's CPU.
	QEMU, Firmware, Kernel, Rootfs string
	Arch                           microvm.Arch
	// ImageEnv is the runner image's own environment (PATH, HOME, ...), which a container exec inherits and a guest
	// exec doesn't: every exec gets it, under its own variables.
	ImageEnv map[string]string
	// StateDir holds one folder per VM (Spec.Dir); CPUs and Memory limit the VM ("" = microvm's defaults).
	StateDir     string
	CPUs, Memory string

	// name and shares, when set, replace the project's name and Project.Mounts: a T3 session's VM, whose shares are its
	// workspace and Home's files at their host paths (t3vm.SessionSpec).
	name   string
	shares []microvm.Share

	// ready says whether the VM can start now (the backend: its files installed, KVM usable); nil: always.
	ready func() error
	// launch starts the VM; a seam for tests (nil: Launcher.Start).
	launch func(microvm.Spec) (microVMProcess, error)

	mu     sync.Mutex
	proj   Project
	vm     microVMProcess
	client *guest.Client
	pipe   *pipeSup
	state  State
}

// microVMProcess is a started VM (*microvm.VM).
type microVMProcess interface {
	DialControl() (net.Conn, error)
	Stop(grace time.Duration)
	Done() <-chan struct{}
	Err() error
	Console() string
}

// The guest's runner user (the runner image's "rubi") and how long the guest has for each step.
var (
	microVMHello    = 30 * time.Second
	microVMPowerOff = 10 * time.Second
)

func (m *MicroVM) Netless() bool { return true }

func (m *MicroVM) setState(s State) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

// State implements VM.
func (m *MicroVM) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == "" {
		return StateStopped
	}
	return m.state
}

// spec is the microvm.Spec of a project's VM: its shares are Project.Mounts, in order.
func (m *MicroVM) spec(p Project) microvm.Spec {
	name := containerName(p)
	if m.name != "" {
		name = m.name
	}
	s := microvm.Spec{QEMU: m.QEMU, Arch: m.Arch, Name: name, Firmware: m.Firmware, Kernel: m.Kernel, Rootfs: m.Rootfs,
		CPUs: m.CPUs, Memory: m.Memory, Dir: filepath.Join(m.StateDir, name)}
	if m.shares != nil {
		s.Shares = append(s.Shares, m.shares...)
		return s
	}
	for _, mt := range p.Mounts() {
		s.Shares = append(s.Shares, microvm.Share{Source: mt.Source, Target: mt.Target, ReadOnly: mt.ReadOnly})
	}
	return s
}

// Start implements VM: the VM, the guest agent, the shares, the pipe, then the runner.
func (m *MicroVM) Start(ctx context.Context, spec StartSpec) (Conn, error) {
	m.setState(StateStarting)
	fail := func(err error) (Conn, error) {
		m.setState(StateFailed)
		return nil, err
	}
	p := spec.Project
	bad := badMountPath(p.Dir) || badMountPath(p.RoleDir) || badMountPath(p.ClaudeDir) || badMountPath(p.EmptyDir)
	for _, ro := range p.ReadOnly {
		bad = bad || badMountPath(ro.Name) || badMountPath(ro.Source)
	}
	if bad {
		return fail(errors.New("the project path can't be mounted (a comma or control character in it)"))
	}
	if r := p.Role; r != "" && (p.RoleDir == "" || p.ClaudeDir == "") {
		return fail(fmt.Errorf("role %s has no directories to mount", r))
	} else if r != "" && p.EmptyDir == "" { // fail closed, as Apple's
		return fail(fmt.Errorf("role %s has no empty directory to cover /work/.rubi with", r))
	}
	if spec.Pipe == nil {
		return fail(errors.New("a VM without a network needs its pipe"))
	}
	if m.ready != nil {
		if err := m.ready(); err != nil {
			return fail(err)
		}
	}
	if err := m.boot(ctx, p); err != nil {
		return fail(err)
	}
	stop := func() { _ = m.Stop(context.Background()) }
	pipe, err := startPipeWith(ctx, containerName(p), m.exec, spec.Pipe, nil, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.Stop(sctx)
	})
	if err != nil {
		stop()
		return fail(fmt.Errorf("the network pipe: %w", err))
	}
	m.mu.Lock()
	m.pipe = pipe
	m.mu.Unlock()
	env, err := prepareEnv(ctx, spec, "")
	if err == nil && p.Role != "" {
		env = withRoleEnv(env)
	}
	if err != nil {
		stop()
		return fail(err)
	}
	c, err := m.execLogged(ctx, env, []string{"node", runnerMain}, true)
	if err != nil {
		stop()
		return fail(err)
	}
	m.setState(StateRunning)
	return c, nil
}

// boot starts the VM, waits for the guest agent and mounts the shares. A failure leaves nothing running.
func (m *MicroVM) boot(ctx context.Context, p Project) error {
	spec := m.spec(p)
	if err := ensureStateDir(m.StateDir); err != nil {
		return err
	}
	launch := m.launch
	if launch == nil {
		launch = func(s microvm.Spec) (microVMProcess, error) { return m.Launcher.Start(s) }
	}
	vm, err := launch(spec)
	if err != nil {
		return err
	}
	conn, err := vm.DialControl()
	if err != nil {
		vm.Stop(0)
		return fmt.Errorf("the VM's control channel: %w", err)
	}
	c := guest.NewClient(conn, log.Printf)
	hctx, cancel := context.WithTimeout(ctx, microVMHello)
	err = c.Ready(hctx)
	cancel()
	if err == nil {
		mounts := make([]guest.Mount, 0, len(spec.Shares))
		for _, gm := range microvm.GuestMounts(spec) {
			mounts = append(mounts, guest.Mount{Tag: gm.Tag, Target: gm.Target, ReadOnly: gm.ReadOnly})
		}
		err = c.Mount(ctx, mounts)
	}
	if err != nil {
		c.Close()
		vm.Stop(0)
		return fmt.Errorf("the VM didn't come up: %w (console: %s)", err, tail([]byte(vm.Console()), 400))
	}
	m.mu.Lock()
	m.proj, m.vm, m.client = p, vm, c
	m.mu.Unlock()
	go func() { // the VM ending by itself (a crash, a kill) ends its state
		<-vm.Done()
		m.mu.Lock()
		if m.vm == vm {
			m.state = StateStopped
			if err := vm.Err(); err != nil {
				m.state = StateFailed
				log.Printf("agent: the VM of %s ended: %v", containerName(p), err)
			}
		}
		m.mu.Unlock()
	}()
	return nil
}

// exec runs argv in the VM as the runner's user in /work, with the image's environment under env.
func (m *MicroVM) exec(ctx context.Context, env map[string]string, argv []string) (Conn, error) {
	return m.execLogged(ctx, env, argv, true)
}

func (m *MicroVM) execLogged(ctx context.Context, env map[string]string, argv []string, logStderr bool) (Conn, error) {
	var stderr io.Writer
	if logStderr {
		stderr = &stderrLog{}
	}
	return m.execIn(ctx, "/work", env, argv, stderr)
}

// execIn runs argv in the VM as the runner's user in dir, with the image's environment under env; its stderr goes to
// stderr (nil drops it).
func (m *MicroVM) execIn(ctx context.Context, dir string, env map[string]string, argv []string, stderr io.Writer) (*guest.Process, error) {
	m.mu.Lock()
	c, role := m.client, m.proj.Role
	m.mu.Unlock()
	if c == nil || len(argv) == 0 {
		return nil, errors.New("the VM isn't running")
	}
	// what Apple's container gives every exec from its own environment: the image's, and a role VM's role folders
	all := make(map[string]string, len(m.ImageEnv)+len(env)+2)
	for k, v := range m.ImageEnv {
		all[k] = v
	}
	if role != "" {
		all = withRoleEnv(all)
	}
	for k, v := range env {
		all[k] = v
	}
	uid, _ := strconv.Atoi(runnerUID)
	return c.Exec(ctx, guest.ExecRequest{Argv: argv, Env: all, Dir: dir, UID: uid, GID: microvm.GuestGID}, stderr)
}

// Exec implements VM; the process's stderr is dropped (a sign-in may print secrets).
func (m *MicroVM) Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error) {
	return m.execLogged(ctx, env, argv, false)
}

// Stop implements VM: the guest powers the VM off; QEMU is ended if it doesn't.
func (m *MicroVM) Stop(ctx context.Context) error {
	m.mu.Lock()
	vm, c, pipe := m.vm, m.client, m.pipe
	m.vm, m.client, m.pipe = nil, nil, nil
	m.mu.Unlock()
	pipe.stop()
	m.setState(StateStopped)
	if vm == nil {
		return nil
	}
	if c != nil {
		pctx, cancel := context.WithTimeout(ctx, microVMPowerOff)
		_ = c.PowerOff(pctx)
		cancel()
		c.Close()
	}
	vm.Stop(microVMPowerOff)
	return nil
}

// stderrLog sends a process's stderr to the log as Apple's backend does: line by line, a line at most 16 KiB (the rest
// of a longer one is dropped), and never a line that looks secret.
type stderrLog struct {
	mu   sync.Mutex
	line []byte
	long bool // the current line passed the limit: dropped up to its end
}

const stderrLineMax = 16 << 10

// logPrintf is where stderrLog writes (a seam for tests).
var logPrintf = log.Printf

func (w *stderrLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		part := b
		if i >= 0 {
			part = b[:i]
		}
		if !w.long {
			if len(w.line)+len(part) > stderrLineMax {
				w.line, w.long = w.line[:0], true
			} else {
				w.line = append(w.line, part...)
			}
		}
		if i < 0 {
			break
		}
		if !w.long && !LooksSecret(string(w.line)) {
			logPrintf("agent runner: %s", w.line)
		}
		w.line, w.long = w.line[:0], false
		b = b[i+1:]
	}
	return n, nil
}

// ensureStateDir makes the folder of the VMs' folders (0700, with its missing parents) on a fresh Home, which has none
// yet; microvm.PrepareDir makes each VM's own folder inside it and needs it there. A link or a file there is refused.
func ensureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the VMs' folder: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the VMs' folder: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("the VMs' folder %s is not a folder", dir)
	}
	return nil
}
