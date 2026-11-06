//go:build linux && kvmlive

package live

import (
	"bufio"
	"bytes"
	"debug/elf"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
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
	if len(os.Args) > 1 && os.Args[1] == "-name" { // QEMU through TestLiveLauncher's wrapper
		qemuWrapper(os.Args[1:])
	}
	if len(os.Args) > 1 && os.Args[1] == "-live-confine" { // the launcher's confining helper (Home's vm-confine)
		err := microvm.ConfineMain(os.Args[2:], os.Getenv(microvm.ConfineEnv))
		fmt.Fprintln(os.Stderr, "vm-confine:", err)
		os.Exit(2)
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
// RUBI_LIVE_VIRTIOFSD (/usr/libexec/virtiofsd) say what to run, RUBI_LIVE_BIOS the firmware (/usr/share/qemu/qboot.rom). The microvm machine boots without option ROMs, which
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
	exact := variant{wait: 60 * time.Second}
	if os.Getenv("RUBI_LIVE_DEBUG") != "" {
		// Where a VM that shows nothing stops: the kernel's own messages from the start, then the same with the legacy
		// timers back or without ACPI. QEMU is asked where the CPU is before it is stopped.
		v := variant{verbose: true, wait: 20 * time.Second}
		t.Run("verbose", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, v) })
		v.machine = map[string]string{"pit": "on", "pic": "on", "rtc": "on"}
		t.Run("verbose-legacy-timers", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, v) })
		v.machine = map[string]string{"acpi": "off"}
		t.Run("verbose-no-acpi", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, v) })
		return
	}
	if isBzImage(t, kernel) {
		t.Logf("%s is a bzImage (no PVH entry): the exact command line is expected to fail to boot it", kernel)
		t.Run("exact", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, exact) })
		roms := exact
		roms.machine = map[string]string{"x-option-roms": "on"}
		t.Run("option-roms-on", func(t *testing.T) { boot(t, qemu, virtiofsd, kernel, roms) })
		return
	}
	boot(t, qemu, virtiofsd, kernel, exact)
}

// variant is how a boot departs from the exact command line, for finding out why one fails.
type variant struct {
	machine map[string]string // -machine options set to other values (or added)
	verbose bool              // the kernel's messages from the start: no "quiet", early console on the serial port
	wait    time.Duration     // how long the VM may run
}

func (v variant) apply(args []string, qmp string) []string {
	out := slices.Clone(args)
	for i := 1; i < len(out); i++ {
		switch out[i-1] {
		case "-machine":
			opts := strings.Split(out[i], ",")
			for k, val := range v.machine {
				found := false
				for j, o := range opts {
					if strings.HasPrefix(o, k+"=") {
						opts[j], found = k+"="+val, true
					}
				}
				if !found {
					opts = append(opts, k+"="+val)
				}
			}
			out[i] = strings.Join(opts, ",")
		case "-append":
			if v.verbose {
				out[i] = strings.Replace(out[i], " quiet", "", 1) + " earlyprintk=ttyS0,115200 ignore_loglevel"
			}
		}
	}
	if v.verbose {
		out = append(out, "-qmp", "unix:"+qmp+",server=on,wait=off")
	}
	return out
}

// whereIsTheCPU asks QEMU (QMP) for the VM's state and the CPU's registers.
func whereIsTheCPU(qmp string) string {
	c, err := net.DialTimeout("unix", qmp, 2*time.Second)
	if err != nil {
		return fmt.Sprintf("qmp: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	var out strings.Builder
	readReply := func() {
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				fmt.Fprintf(&out, "qmp: %v\n", err)
				return
			}
			if strings.Contains(l, `"return"`) || strings.Contains(l, `"error"`) {
				out.WriteString(strings.ReplaceAll(l, `\r\n`, "\n"))
				return
			}
		}
	}
	_, _ = r.ReadString('\n') // greeting
	for _, cmd := range []string{
		`{"execute":"qmp_capabilities"}`,
		`{"execute":"query-status"}`,
		`{"execute":"human-monitor-command","arguments":{"command-line":"info registers"}}`,
	} {
		_, _ = c.Write([]byte(cmd + "\n"))
		readReply()
	}
	return out.String()
}

// waitKVM waits until /dev/kvm opens for reading and writing: on a shared test machine its access is sometimes
// withdrawn for a while between runs.
func waitKVM(t *testing.T) {
	if os.Getenv("RUBI_LIVE_TCG") != "" {
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err == nil {
			f.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/dev/kvm: %v", err)
		}
		time.Sleep(time.Second)
	}
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

func boot(t *testing.T, qemu, virtiofsd, kernel string, v variant) {
	waitKVM(t)
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
		Firmware: env("RUBI_LIVE_BIOS", "/usr/share/qemu/qboot.rom"),
		CPUs:     "1", Memory: "256M", Dir: vm,
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
	qmp := filepath.Join(vm, "qmp.sock")
	args = v.apply(args, qmp)
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
	case <-time.After(v.wait):
		if v.verbose {
			t.Logf("QEMU (QMP) before it is stopped:\n%s", whereIsTheCPU(qmp))
		}
		_ = q.cmd.Process.Kill()
		<-q.done
		t.Errorf("QEMU still ran after %v: the guest's restart did not end it", v.wait)
	}
	took := time.Since(began)
	conn.Close()
	<-read
	t.Logf("QEMU exit: %v after %v", q.err, took.Round(time.Millisecond))
	t.Logf("QEMU said:\n%s", q.out())
	t.Logf("console (lines from the probe):\n%s", probeLines(console.String()))
	if v.verbose {
		t.Logf("console (its end):\n%s", tail(console.String(), 6000))
	}
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
		st := fi.Sys().(*syscall.Stat_t)
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
	// RUBI_LIVE_INIT: another build of this test for the guest's CPU, when it isn't the host's (an emulated guest)
	self = env("RUBI_LIVE_INIT", self)
	exe, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	// The initramfs has nothing but this binary: a dynamically linked one finds no loader, and the kernel falls back to
	// init= and panics.
	if f, err := elf.Open(self); err == nil {
		for _, p := range f.Progs {
			if p.Type == elf.PT_INTERP {
				f.Close()
				t.Fatal("the test binary is dynamically linked: run the test with CGO_ENABLED=0")
			}
		}
		f.Close()
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

// qemuWrapper is the QEMU the launcher starts in TestLiveLauncher: the real one, with the probe's initramfs added (the
// backend's VMs boot from their root disk and have none). Both are named in files beside the VM's folder, since the
// launcher gives QEMU an empty environment.
func qemuWrapper(args []string) {
	var dir string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-pidfile" {
			dir = filepath.Dir(filepath.Dir(args[i+1]))
		}
	}
	real, err := os.ReadFile(filepath.Join(dir, "qemu-path"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "wrapper:", err)
		os.Exit(1)
	}
	q := strings.TrimSpace(string(real))
	if _, err := os.Stat(filepath.Join(dir, "tcg")); err == nil {
		// no KVM here (RUBI_LIVE_TCG): QEMU's emulator, the rest of the command line as it is
		for i, a := range args {
			if strings.HasPrefix(a, "microvm,") || strings.HasPrefix(a, "virt,") {
				// the emulator has no kvmclock: the guest needs the legacy timers to keep time
				args[i] = strings.NewReplacer("accel=kvm", "accel=tcg", "pit=off", "pit=on", "pic=off", "pic=on", "rtc=off", "rtc=on").Replace(a)
			}
			if i > 0 && args[i-1] == "-append" {
				args[i] = strings.Replace(a, " quiet", "", 1) + " earlyprintk=ttyS0,115200 ignore_loglevel"
			}
			if a == "host" && i > 0 && args[i-1] == "-cpu" {
				args[i] = "max"
			}
		}
	}
	err = unix.Exec(q, append(append([]string{q}, args...), "-initrd", filepath.Join(dir, "initrd.cpio")), os.Environ())
	fmt.Fprintln(os.Stderr, "wrapper:", err)
	os.Exit(1)
}

// TestLiveLauncher boots the same VM through microvm.Launcher, as the backend will: each virtiofsd confined by Landlock
// to its share (through the confining helper), pid files, the console kept by the launcher, and the folder cleared at
// the end. The library folders of a distribution's virtiofsd are allowed (the kernel's exec checks its loader too).
func TestLiveLauncher(t *testing.T) {
	if _, err := microvm.LandlockABI(); err != nil {
		t.Fatalf("Landlock: %v", err)
	}
	var u unix.Utsname
	_ = unix.Uname(&u)
	kernel := env("RUBI_LIVE_KERNEL", "/boot/vmlinuz-"+unix.ByteSliceToString(u.Release[:]))
	if isBzImage(t, kernel) {
		t.Fatalf("%s is a bzImage: give the ELF vmlinux with a PVH entry", kernel)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "rubi-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	rw, ro := filepath.Join(dir, "rw"), filepath.Join(dir, "ro")
	for _, d := range []string{rw, ro} {
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
	virtiofsd := env("RUBI_LIVE_VIRTIOFSD", "/usr/libexec/virtiofsd")
	var libs []string
	// a pinned build's own libraries (bin/virtiofsd beside lib/), then the system's (glibc and its loader)
	for _, l := range []string{filepath.Join(filepath.Dir(filepath.Dir(virtiofsd)), "lib"), "/lib", "/lib64", "/usr/lib", "/usr/lib64"} {
		if fi, err := os.Stat(l); err == nil && fi.IsDir() && !slices.Contains(libs, l) {
			libs = append(libs, l)
		}
	}
	l := microvm.Launcher{
		Virtiofsd:  virtiofsd,
		Libs:       libs,
		Confine:    []string{self, "-live-confine"},
		IDs:        microvm.HostIDs{UID: os.Getuid(), GID: os.Getgid()},
		SocketWait: 10 * time.Second,
	}
	if err := os.WriteFile(filepath.Join(dir, "qemu-path"), []byte(env("RUBI_LIVE_QEMU", "/usr/bin/qemu-system-x86_64")), 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("RUBI_LIVE_TCG") != "" {
		_ = os.WriteFile(filepath.Join(dir, "tcg"), nil, 0o600)
	}
	spec := microvm.Spec{QEMU: self, Arch: microvm.X86_64, Name: "rubi-live",
		Firmware: env("RUBI_LIVE_BIOS", "/usr/share/qemu/qboot.rom"), Kernel: kernel, Rootfs: rootfs,
		CPUs: "1", Memory: "256M", Dir: filepath.Join(dir, "vm"),
		Shares: []microvm.Share{{Source: rw, Target: "/work"}, {Source: ro, Target: "/ref", ReadOnly: true}}}
	t.Logf("libraries allowed to virtiofsd: %q", libs)
	waitKVM(t)
	began := time.Now()
	vm, err := l.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-vm.Done():
	case <-time.After(180 * time.Second):
		vm.Stop(0)
		t.Errorf("the VM still ran after 60 s")
	}
	t.Logf("ended after %v: %v", time.Since(began).Round(time.Millisecond), vm.Err())
	t.Logf("console (lines from the probe):\n%s", probeLines(vm.Console()))
	if err := vm.Err(); err != nil {
		t.Errorf("ended with %v; console tail:\n%s", err, tail(vm.Console(), 16000))
	}
	if !strings.Contains(vm.Console(), markReboot) {
		t.Errorf("the probe never got to restart; console tail:\n%s", tail(vm.Console(), 16000))
	}
	ents, _ := os.ReadDir(spec.Dir)
	for _, e := range ents {
		t.Errorf("left in the VM folder: %s", e.Name())
	}
	// a kernel with virtio-fs (Rubi's) mounts the share: what the guest wrote there is the user's
	if strings.Contains(vm.Console(), "mount rubi0: ok") {
		fi, err := os.Stat(filepath.Join(rw, "from-guest"))
		if err != nil {
			t.Fatalf("the guest's file: %v", err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Errorf("the guest's file is %d:%d, not the user's", st.Uid, st.Gid)
		}
	}
}
