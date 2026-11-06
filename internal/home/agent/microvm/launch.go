//go:build unix

package microvm

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Launcher starts VMs: one QEMU and one virtiofsd per shared folder, as the user.
type Launcher struct {
	// Virtiofsd is the pinned virtiofsd and Libs the folders of the libraries it loads (VirtiofsdRules).
	Virtiofsd string
	Libs      []string
	// Confine is the command that runs a program confined: Home itself with "vm-confine" (ConfineMain).
	Confine []string
	// IDs is the user (virtiofsd's --translate-uid and --translate-gid).
	IDs HostIDs
	// SocketWait is how long a process may take to make its sockets (0: 10 s).
	SocketWait time.Duration
}

// consoleKeep is how much of the console's output a VM keeps: the last bytes, for the log when it fails.
const consoleKeep = 64 << 10

// fsGrace is how long a virtiofsd may take to end after its QEMU did.
const fsGrace = 2 * time.Second

// VM is a started VM.
type VM struct {
	Spec Spec

	qemu    *child
	fsds    []*child
	console tail
	done    chan struct{}
	err     error // set before done is closed
}

// child is a started process with the end of its output.
type child struct {
	name string
	cmd  *exec.Cmd
	out  tail
	done chan struct{}
	err  error
}

func startChild(name string, argv, env []string, extraEnv ...string) (*child, error) {
	c := &child{name: name, done: make(chan struct{}), out: tail{max: 16 << 10}}
	c.cmd = exec.Command(argv[0], argv[1:]...)
	c.cmd.Env = append(env, extraEnv...)
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // a signal to Home's group (^C in a terminal) is not theirs
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	go func() { c.err = c.cmd.Wait(); close(c.done) }()
	return c, nil
}

func (c *child) running() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// end ends the process: SIGTERM, then SIGKILL after grace.
func (c *child) end(grace time.Duration) {
	if !c.running() {
		return
	}
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(grace):
		_ = c.cmd.Process.Kill()
		<-c.done
	}
}

func (c *child) failure() error {
	return fmt.Errorf("%s ended (%v): %s", c.name, c.err, c.out.String())
}

// waitSocket waits until path is a socket, the process ends, or the time is up.
func waitSocket(c *child, path string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if !c.running() {
			return c.failure()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s made no %s in %v", c.name, filepath.Base(path), d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// dialSocket connects to a socket of the process. The file is there from the moment the process binds it, a little
// before it listens, so a refused connection is tried again until the process ends or the time is up.
func dialSocket(c *child, path string, d time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(d)
	for {
		conn, err := net.Dial("unix", path)
		if err == nil {
			return conn, nil
		}
		if !c.running() {
			return nil, c.failure()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: %s: %w", c.name, filepath.Base(path), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Start starts spec's VM: it prepares the VM's folder, starts each share's virtiofsd confined to its folder (and writes
// its pid file), waits for their sockets, then starts QEMU and connects to its console. On any failure what it started
// is ended and the folder cleared.
func (l Launcher) Start(spec Spec) (_ *VM, err error) {
	if len(l.Confine) == 0 {
		return nil, fmt.Errorf("%w: no confining helper", ErrSpec)
	}
	qargs, err := QEMUArgs(spec)
	if err != nil {
		return nil, err
	}
	wait := l.SocketWait
	if wait == 0 {
		wait = 10 * time.Second
	}
	if err := PrepareDir(spec.Dir); err != nil {
		return nil, err
	}
	vm := &VM{Spec: spec, done: make(chan struct{}), console: tail{max: consoleKeep}}
	defer func() {
		if err != nil {
			vm.kill()
			_ = clearDir(spec.Dir)
		}
	}()
	for i := range spec.Shares {
		args, err := VirtiofsdArgs(l.Virtiofsd, spec, i, l.IDs)
		if err != nil {
			return nil, err
		}
		rules, err := VirtiofsdRules(l.Virtiofsd, spec, i, l.Libs)
		if err != nil {
			return nil, err
		}
		js, _ := json.Marshal(rules)
		argv := append(append(append([]string{}, l.Confine...), "--"), args...)
		c, err := startChild(fmt.Sprintf("the file server of %s", spec.Shares[i].Source), argv, []string{}, ConfineEnv+"="+string(js))
		if err != nil {
			return nil, err
		}
		vm.fsds = append(vm.fsds, c)
		if err := writePid(filepath.Join(spec.Dir, sharePidFile(i)), c.cmd.Process.Pid); err != nil {
			return nil, err
		}
		if err := waitSocket(c, filepath.Join(spec.Dir, ShareSocket(i)), wait); err != nil {
			return nil, err
		}
	}
	q, err := startChild("QEMU", qargs, []string{})
	if err != nil {
		return nil, err
	}
	vm.qemu = q
	if err := waitSocket(q, filepath.Join(spec.Dir, ConsoleSocket), wait); err != nil {
		return nil, err
	}
	conn, err := dialSocket(q, filepath.Join(spec.Dir, ConsoleSocket), wait)
	if err != nil {
		return nil, err
	}
	if err := waitSocket(q, filepath.Join(spec.Dir, ControlSocket), wait); err != nil {
		conn.Close()
		return nil, err
	}
	go func() { _, _ = vm.console.drain(conn); conn.Close() }()
	go vm.watch()
	return vm, nil
}

// watch ends the VM as a whole: when QEMU ends, the file servers go too; when a file server ends first, its share is
// gone and QEMU is ended (a VM must not run on without a folder it was given).
func (vm *VM) watch() {
	fsEnded := make(chan *child, len(vm.fsds))
	for _, c := range vm.fsds {
		go func() { <-c.done; fsEnded <- c }()
	}
	var err error
	select {
	case <-vm.qemu.done:
		if vm.qemu.err != nil {
			err = vm.qemu.failure()
		}
	case c := <-fsEnded:
		err = c.failure()
		vm.qemu.end(fsGrace)
	}
	for _, c := range vm.fsds {
		select {
		case <-c.done:
		case <-time.After(fsGrace):
			c.end(fsGrace)
		}
	}
	_ = clearDir(vm.Spec.Dir)
	vm.err = err
	close(vm.done)
}

// kill ends every process at once (a failed start).
func (vm *VM) kill() {
	for _, c := range append([]*child{vm.qemu}, vm.fsds...) {
		if c != nil && c.running() {
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	}
}

// Done is closed when the VM has ended (QEMU and its file servers), and Err then says why if it wasn't QEMU ending well.
func (vm *VM) Done() <-chan struct{} { return vm.done }

// Err is why the VM ended, once Done is closed: nil when QEMU ended with status 0 (the guest's restart).
func (vm *VM) Err() error {
	select {
	case <-vm.done:
		return vm.err
	default:
		return nil
	}
}

// Console is the end of what the VM wrote on its console (at most 64 KiB).
func (vm *VM) Console() string { return vm.console.String() }

// DialControl connects to the VM's control channel (the guest agent's port), waiting a little for QEMU to listen.
func (vm *VM) DialControl() (net.Conn, error) {
	return dialSocket(vm.qemu, filepath.Join(vm.Spec.Dir, ControlSocket), 2*time.Second)
}

// Stop ends the VM: it waits grace for it to end by itself (the caller asked the guest to power off), then ends QEMU
// (SIGTERM, then SIGKILL), and returns once everything has ended and the folder is cleared.
func (vm *VM) Stop(grace time.Duration) {
	select {
	case <-vm.done:
		return
	case <-time.After(grace):
	}
	vm.qemu.end(fsGrace)
	<-vm.done
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tail) drain(r interface{ Read([]byte) (int, error) }) (int64, error) {
	var n int64
	buf := make([]byte, 4096)
	for {
		k, err := r.Read(buf)
		if k > 0 {
			_, _ = t.Write(buf[:k])
			n += int64(k)
		}
		if err != nil {
			return n, err
		}
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
