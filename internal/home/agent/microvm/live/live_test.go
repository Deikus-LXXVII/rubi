//go:build linux && kvmlive

package live

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/guest"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

// The test binary is also the VM's init: it is copied into the initramfs as /init, and as PID 1 it runs probe instead of
// the tests.
func TestMain(m *testing.M) {
	if os.Getpid() == 1 {
		probe()
	}
	os.Exit(m.Run())
}

// Lines the probe writes on the console.
const (
	mark       = "RUBI-LIVE "
	markInit   = mark + "init"
	markReboot = mark + "restarting"
)

// probe is PID 1 in the VM: it says it runs, reports what the guest agent would find (the control port, virtio-fs), and
// ends the VM the way the guest agent does (guest.LinuxServer's PowerOff).
func probe() {
	say := func(format string, args ...any) { fmt.Fprintf(os.Stdout, mark+format+"\n", args...) }
	say("init")
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		say("kernel %s", unix.ByteSliceToString(u.Release[:]))
	}
	if err := guest.InitMounts(); err != nil {
		say("mounts: %v", err)
	} else {
		say("mounts: ok")
	}
	if p, err := guest.FindPort("/sys", "/dev", guest.ControlPortName); err != nil {
		say("control port: %v", err)
	} else {
		say("control port: %s", p)
	}
	if b, err := os.ReadFile("/proc/filesystems"); err == nil {
		say("virtiofs in kernel: %v", bytes.Contains(b, []byte("virtiofs")))
	}
	if err := guest.MountShare(guest.Mount{Tag: microvm.ShareTag(0), Target: "/mnt/w"}); err != nil {
		say("mount rubi0: %v", err)
	} else {
		err := os.WriteFile("/mnt/w/from-guest", []byte("hello\n"), 0o644)
		say("mount rubi0: ok, write: %v", err)
	}
	say("restarting")
	if err := guest.LinuxServer().PowerOff(); err != nil {
		say("power off: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// TestLiveVM boots a VM with microvm.QEMUArgs and microvm.VirtiofsdArgs exactly (plus -initrd for the probe) and checks
// that QEMU accepts every option, that the virtiofsd processes take their flags and their vhost-user connection, that
// the console socket carries the guest's output, and that the guest's restart ends QEMU with status 0.
//
// RUBI_LIVE_KERNEL (default /boot/vmlinuz-<this kernel>), RUBI_LIVE_QEMU (/usr/bin/qemu-system-x86_64) and
// RUBI_LIVE_VIRTIOFSD (/usr/libexec/virtiofsd) say what to run. The microvm machine boots without option ROMs, which
// takes a kernel with a PVH entry (an ELF vmlinux); for a compressed bzImage the test also tries option ROMs on, to tell
// the kernel's format from the rest.
func TestLiveVM(t *testing.T) {
	var u unix.Utsname
	_ = unix.Uname(&u)
	kernel := env("RUBI_LIVE_KERNEL", "/boot/vmlinuz-"+unix.ByteSliceToString(u.Release[:]))
	qemu := env("RUBI_LIVE_QEMU", "/usr/bin/qemu-system-x86_64")
	virtiofsd := env("RUBI_LIVE_VIRTIOFSD", "/usr/libexec/virtiofsd")
	for _, p := range []string{kernel, qemu, virtiofsd} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%v", err)
		}
	}
	if isBzImage(t, kernel) {
		t.Logf("%s is a bzImage (no PVH entry): the exact command line is expected to fail to boot it", kernel)
		t.Run("exact", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, false) })
		t.Run("option-roms-on", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, true) })
		return
	}
	boot(t, qemu, virtiofsd, kernel, false)
}

func isBzImage(t *testing.T, kernel string) bool {
	f, err := os.Open(kernel)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var h [4]byte
	if _, err := f.ReadAt(h[:], 0x202); err != nil {
		return false
	}
	return string(h[:]) == "HdrS"
}

func boot(t *testing.T, qemu, virtiofsd, kernel string, optionROMs bool) {
	dir, err := os.MkdirTemp("", "rubi-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	vm := filepath.Join(dir, "vm")
	rw, ro := filepath.Join(dir, "rw"), filepath.Join(dir, "ro")
	for _, d := range []string{vm, rw, ro} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	rootfs := filepath.Join(dir, "rootfs.img")
	if err := os.WriteFile(rootfs, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	initrd := filepath.Join(dir, "initrd.cpio")
	writeInitrd(t, initrd)

	spec := microvm.Spec{QEMU: qemu, Arch: microvm.X86_64, Name: "rubi-live", Kernel: kernel, Rootfs: rootfs,
		CPUs: "1", Memory: "256M", Dir: vm,
		Shares: []microvm.Share{{Source: rw, Target: "/work"}, {Source: ro, Target: "/ref", ReadOnly: true}}}

	ids := microvm.HostIDs{UID: os.Getuid(), GID: os.Getgid()}
	var fsds []*proc
	for i := range spec.Shares {
		args, err := microvm.VirtiofsdArgs(virtiofsd, spec, i, ids)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("virtiofsd %d: %q", i, args[1:])
		p := start(t, args)
		fsds = append(fsds, p)
		if !waitFor(func() bool { return isSocket(filepath.Join(vm, microvm.ShareSocket(i))) }, 5*time.Second, p) {
			t.Fatalf("virtiofsd %d made no socket; it said:\n%s", i, p.out())
		}
	}

	args, err := microvm.QEMUArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if optionROMs {
		for i, a := range args {
			args[i] = strings.Replace(a, "x-option-roms=off", "x-option-roms=on", 1)
		}
	}
	args = append(args, "-initrd", initrd)
	t.Logf("qemu: %q", args[1:])
	began := time.Now()
	q := start(t, args)
	if !waitFor(func() bool { return isSocket(filepath.Join(vm, microvm.ConsoleSocket)) }, 10*time.Second, q) {
		t.Fatalf("QEMU made no console socket; it said:\n%s", q.out())
	}
	console := &bounded{max: 1 << 20}
	conn, err := net.Dial("unix", filepath.Join(vm, microvm.ConsoleSocket))
	if err != nil {
		// QEMU made the socket and ended at once: say why
		select {
		case <-q.done:
		case <-time.After(5 * time.Second):
		}
		t.Fatalf("console: %v; QEMU: %v, it said:\n%s", err, q.err, q.out())
	}
	read := make(chan struct{})
	go func() { _, _ = io.Copy(console, conn); close(read) }()
	if _, err := os.Stat(filepath.Join(vm, microvm.PidFile)); err != nil {
		t.Errorf("pid file: %v", err)
	}

	select {
	case <-q.done:
	case <-time.After(60 * time.Second):
		_ = q.cmd.Process.Kill()
		<-q.done
		t.Errorf("QEMU still ran after 60 s: the guest's restart did not end it")
	}
	took := time.Since(began)
	conn.Close()
	<-read
	t.Logf("QEMU exit: %v after %v", q.err, took.Round(time.Millisecond))
	t.Logf("QEMU said:\n%s", q.out())
	t.Logf("console (lines from the probe):\n%s", probeLines(console.String()))
	if q.err != nil {
		t.Errorf("QEMU ended with %v", q.err)
	}
	c := console.String()
	if !strings.Contains(c, markInit) {
		t.Errorf("the probe never ran; console tail:\n%s", tail(c, 3000))
	}
	if !strings.Contains(c, markReboot) {
		t.Errorf("the probe never got to restart")
	}

	// virtiofsd ends when its QEMU goes
	for i, p := range fsds {
		select {
		case <-p.done:
			t.Logf("virtiofsd %d ended: %v", i, p.err)
		case <-time.After(5 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
			t.Errorf("virtiofsd %d still ran 5 s after QEMU ended; it said:\n%s", i, p.out())
		}
	}
	if b, err := os.ReadFile(filepath.Join(rw, "from-guest")); err == nil {
		fi, _ := os.Stat(filepath.Join(rw, "from-guest"))
		st := fi.Sys().(*unix.Stat_t)
		t.Logf("the guest wrote %q; on this computer it is uid %d gid %d", b, st.Uid, st.Gid)
		if int(st.Uid) != ids.UID || int(st.Gid) != ids.GID {
			t.Errorf("the guest's file is not the user's")
		}
	}
}

type proc struct {
	cmd  *exec.Cmd
	buf  *bounded
	done chan struct{}
	err  error
}

func (p *proc) out() string { return p.buf.String() }

func start(t *testing.T, args []string) *proc {
	p := &proc{buf: &bounded{max: 64 << 10}, done: make(chan struct{})}
	p.cmd = exec.Command(args[0], args[1:]...)
	p.cmd.Stdout, p.cmd.Stderr = p.buf, p.buf
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func waitFor(ok func() bool, d time.Duration, p *proc) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		select {
		case <-p.done:
			return ok()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return false
}

func isSocket(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// bounded keeps at most max bytes (the first ones).
type bounded struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (w *bounded) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if room := w.max - len(w.b); room > 0 {
		w.b = append(w.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (w *bounded) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}

func probeLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if i := strings.Index(l, mark); i >= 0 {
			out = append(out, strings.TrimRight(l[i:], "\r"))
		}
	}
	return strings.Join(out, "\n")
}

func tail(s string, n int) string { return s[max(0, len(s)-n):] }

// writeInitrd writes a newc cpio archive with this test binary as /init, /dev/console (so init has a console) and the
// folders guest.InitMounts mounts on.
func writeInitrd(t *testing.T, path string) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	ino := 1
	add := func(name string, mode uint32, rdevMajor, rdevMinor int, data []byte) {
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, 1, 0, len(data), 0, 0, rdevMajor, rdevMinor, len(name)+1, 0)
		ino++
		b.WriteString(name)
		b.WriteByte(0)
		pad(&b)
		b.Write(data)
		pad(&b)
	}
	for _, d := range []string{"dev", "proc", "sys", "tmp", "run", "mnt"} {
		add(d, unix.S_IFDIR|0o755, 0, 0, nil)
	}
	add("dev/console", unix.S_IFCHR|0o600, 5, 1, nil)
	add("init", unix.S_IFREG|0o755, 0, 0, exe)
	add("TRAILER!!!", 0, 0, 0, nil)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pad(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}
