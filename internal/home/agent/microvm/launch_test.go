//go:build unix

package microvm

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rig is a VM of fake programs (main_test.go): two shares, one read-only.
type rig struct {
	l    Launcher
	spec Spec
	root string
}

func newLaunchRig(t *testing.T) *rig {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "rl") // short: the sockets' paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { os.RemoveAll(root) })
	rw, ro := filepath.Join(root, "rw"), filepath.Join(root, "ro")
	for _, d := range []string{rw, ro} {
		_ = os.Mkdir(d, 0o700)
	}
	confine := []string{self, "-fake-unconfined"}
	var libs []string
	if _, err := LandlockABI(); err == nil {
		confine = []string{self, "-fake-confine"} // the real confinement
		// a dynamically linked program (the race detector's test binary, a distribution's virtiofsd) needs its loader
		// and libraries: the kernel's exec checks the loader too
		for _, l := range []string{"/lib", "/lib64", "/usr/lib"} {
			if fi, err := os.Stat(l); err == nil && fi.IsDir() {
				libs = append(libs, l)
			}
		}
	}
	return &rig{
		root: root,
		l:    Launcher{Virtiofsd: self, Libs: libs, Confine: confine, IDs: HostIDs{UID: 1000, GID: 1000}, SocketWait: 5 * time.Second},
		spec: Spec{QEMU: self, Arch: X86_64, Name: "rubi-test", Firmware: self, Kernel: self, Rootfs: self,
			Dir:    filepath.Join(root, "vm"),
			Shares: []Share{{Source: rw, Target: "/work"}, {Source: ro, Target: "/ref", ReadOnly: true}}},
	}
}

// start starts the VM; whatever a failing test leaves running is killed when it ends.
func (r *rig) start(t *testing.T) (*VM, error) {
	vm, err := r.l.Start(r.spec)
	if vm != nil {
		t.Cleanup(func() {
			vm.kill()
			<-vm.done
		})
	}
	return vm, err
}

func (r *rig) qemuMode(t *testing.T, mode string) {
	_ = os.MkdirAll(r.spec.Dir, 0o700)
	if err := os.WriteFile(filepath.Join(r.spec.Dir, "fake-qemu.mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
}

func alivePid(pid int) bool { return syscall.Kill(pid, 0) == nil }

func pidsOf(vm *VM) []int {
	pids := []int{vm.qemu.cmd.Process.Pid}
	for _, c := range vm.fsds {
		pids = append(pids, c.cmd.Process.Pid)
	}
	return pids
}

func leftFiles(t *testing.T, dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if ourFile.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

func waitDone(t *testing.T, vm *VM, d time.Duration) {
	select {
	case <-vm.Done():
	case <-time.After(d):
		t.Fatalf("the VM still runs after %v", d)
	}
}

// A VM starts with its file servers, its pid files and its console; the guest's restart ends QEMU with status 0, the
// file servers end with it, and the folder is cleared.
func TestLaunchStartsAndEnds(t *testing.T) {
	r := newLaunchRig(t)
	r.qemuMode(t, "exit0")
	vm, err := r.start(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"qemu.pid", "fs0.pid", "fs1.pid", "control.sock"} {
		if _, err := os.Stat(filepath.Join(r.spec.Dir, n)); err != nil && n != "qemu.pid" {
			t.Errorf("%s: %v", n, err)
		}
	}
	if fi, _ := os.Stat(r.spec.Dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("folder %v", fi.Mode().Perm())
	}
	pids := pidsOf(vm)
	waitDone(t, vm, 5*time.Second)
	if err := vm.Err(); err != nil {
		t.Errorf("ended with %v", err)
	}
	if !strings.Contains(vm.Console(), "booted") {
		t.Errorf("console %q", vm.Console())
	}
	for _, p := range pids {
		if alivePid(p) {
			t.Errorf("%d still runs", p)
		}
	}
	if l := leftFiles(t, r.spec.Dir); len(l) != 0 {
		t.Errorf("left %q", l)
	}
	if _, err := os.Stat(filepath.Join(r.root, "escaped")); err == nil {
		t.Error("escaped")
	}
}

// A file server that won't start fails the start with what it said; nothing is left running and the folder is cleared.
func TestLaunchFileServerFailsTheStart(t *testing.T) {
	r := newLaunchRig(t)
	_ = os.WriteFile(filepath.Join(r.spec.Shares[1].Source, ".fake"), []byte("fail"), 0o600)
	vm, err := r.start(t)
	if err == nil {
		vm.Stop(0)
		t.Fatal("started")
	}
	if !strings.Contains(err.Error(), "--bad-flag") {
		t.Errorf("error %v", err)
	}
	if l := leftFiles(t, r.spec.Dir); len(l) != 0 {
		t.Errorf("left %q", l)
	}
}

// QEMU failing fails the start with what it said, and the file servers already started are ended.
func TestLaunchQEMUFailsTheStart(t *testing.T) {
	r := newLaunchRig(t)
	r.qemuMode(t, "fail")
	vm, err := r.start(t)
	if err == nil {
		vm.Stop(0)
		t.Fatal("started")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("error %v", err)
	}
	if pid, err := readPid(filepath.Join(r.spec.Shares[0].Source, ".fakepid")); err != nil || alivePid(pid) {
		t.Errorf("the file server started before QEMU still runs (%d, %v)", pid, err)
	}
	if l := leftFiles(t, r.spec.Dir); len(l) != 0 {
		t.Errorf("left %q", l)
	}
}

// A file server that dies while the VM runs ends the VM: the share is gone.
func TestLaunchFileServerDeathEndsTheVM(t *testing.T) {
	r := newLaunchRig(t)
	r.qemuMode(t, "run")
	_ = os.WriteFile(filepath.Join(r.spec.Shares[0].Source, ".fake"), []byte("die-later"), 0o600)
	vm, err := r.start(t)
	if err != nil {
		t.Fatal(err)
	}
	pids := pidsOf(vm)
	waitDone(t, vm, 10*time.Second)
	if err := vm.Err(); err == nil || !strings.Contains(err.Error(), "file server") {
		t.Errorf("ended with %v", err)
	}
	for _, p := range pids {
		if alivePid(p) {
			t.Errorf("%d still runs", p)
		}
	}
}

// Stop ends a QEMU that ignores SIGTERM: after the grace, SIGKILL.
func TestLaunchStopEndsAStubbornQEMU(t *testing.T) {
	r := newLaunchRig(t)
	r.qemuMode(t, "ignore-term")
	vm, err := r.start(t)
	if err != nil {
		t.Fatal(err)
	}
	pids := pidsOf(vm)
	began := time.Now()
	vm.Stop(100 * time.Millisecond)
	if d := time.Since(began); d > 10*time.Second {
		t.Errorf("Stop took %v", d)
	}
	for _, p := range pids {
		if alivePid(p) {
			t.Errorf("%d still runs", p)
		}
	}
	if l := leftFiles(t, r.spec.Dir); len(l) != 0 {
		t.Errorf("left %q", l)
	}
}

// On Linux each file server runs confined to its folder: it can't write beside it.
func TestLaunchConfinesTheFileServers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Landlock is Linux's")
	}
	if _, err := LandlockABI(); errors.Is(err, ErrNoLandlock) {
		t.Skip("no Landlock")
	}
	r := newLaunchRig(t)
	r.qemuMode(t, "exit0")
	_ = os.WriteFile(filepath.Join(r.spec.Shares[0].Source, ".fake"), []byte("escape"), 0o600)
	vm, err := r.start(t)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, vm, 5*time.Second)
	if err := vm.Err(); err != nil {
		t.Errorf("ended with %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.root, "escaped")); err == nil {
		t.Error("the file server wrote beside its folder")
	}
}

func TestConfineMainRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		args  []string
		rules string
	}{
		"no --":      {[]string{"/bin/true"}, `[{"Path":"/tmp","Access":1}]`},
		"no program": {[]string{"--"}, `[{"Path":"/tmp","Access":1}]`},
		"no rules":   {[]string{"--", "/bin/true"}, ``},
		"empty set":  {[]string{"--", "/bin/true"}, `[]`},
	} {
		if err := ConfineMain(c.args, c.rules); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
