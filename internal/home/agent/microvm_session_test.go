//go:build unix

package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// A session VM: exactly the given folders, in order, at their own paths; its memory is the session size; a process runs
// in the folder asked for, with its stderr apart; Stop ends the VM.
func TestSessionVMSharesRunsInAFolderAndStops(t *testing.T) {
	r := newMicroRig(t)
	mounts := []Mount{{Source: "/home/u/proj", Target: "/home/u/proj"}, {Source: "/h/vm-etc/rubi", Target: "/etc/rubi", ReadOnly: true}}
	vm, err := startSessionVM(context.Background(), r.m, "rubi-t3-r-abc", mounts)
	if err != nil {
		t.Fatal(err)
	}
	want := []microvm.Share{{Source: "/home/u/proj", Target: "/home/u/proj"}, {Source: "/h/vm-etc/rubi", Target: "/etc/rubi", ReadOnly: true}}
	if !slices.Equal(r.spec.Shares, want) || r.spec.Name != "rubi-t3-r-abc" || r.spec.Memory != "1024" {
		t.Errorf("spec %+v", r.spec)
	}
	proxy := netpipe.NewListener("proxy", 4)
	defer proxy.Close()
	stop, err := vm.StartPipe(context.Background(), &PipeSpec{Proxy: proxy}, nil, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var stderr strings.Builder
	p, err := vm.ExecIn(context.Background(), "/home/u/proj", map[string]string{"A": "1"}, []string{"sh", "-c", "echo out; echo err >&2; exit 3"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p)
	code, _ := p.Wait()
	if string(out) != "out\n" || code != 3 {
		t.Errorf("out %q code %d", out, code)
	}
	r.fake.mu.Lock()
	last := r.fake.execs[len(r.fake.execs)-1]
	r.fake.mu.Unlock()
	if last.Dir != "/home/u/proj" || last.Env["A"] != "1" || last.Env["HOME"] != "/tmp" || last.UID != 10001 {
		t.Errorf("exec %+v", last)
	}
	if err := vm.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.fake.mu.Lock()
	stopped := r.fake.stopped
	r.fake.mu.Unlock()
	if !stopped {
		t.Error("the VM runs on after Stop")
	}
}

func TestSessionVMRefusesAnUnmountablePath(t *testing.T) {
	r := newMicroRig(t)
	launched := false
	r.m.launch = func(microvm.Spec) (microVMProcess, error) { launched = true; return r.fake, nil }
	if _, err := startSessionVM(context.Background(), r.m, "rubi-t3-x", []Mount{{Source: "/a,b", Target: "/a,b"}}); err == nil || launched {
		t.Errorf("err %v, launched %v", err, launched)
	}
}

func TestSessionHostNeedsFilesKVMAndMemory(t *testing.T) {
	b := &MicroVMBackend{files: microVMFiles{Root: t.TempDir(), Version: "v1", Arch: microvm.X86_64},
		kvm: func() (bool, string) { return true, "" }}
	h := b.StreamSessions()
	if err := h.Ready(); err == nil || !strings.Contains(err.Error(), "VM files") {
		t.Errorf("no files: %v", err)
	}
	if err := os.MkdirAll(b.files.dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.files.dir(), microVMComplete), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.memAvailable = func() (int, bool) { return SessionVMMemory + sessionMemoryMargin - 1, true }
	if err := h.Ready(); err == nil || !strings.Contains(err.Error(), "MiB of memory") {
		t.Errorf("too little memory: %v", err)
	}
	if _, err := h.StartSessionVM(context.Background(), "rubi-t3-x", nil); err == nil {
		t.Error("a session VM started with too little memory")
	}
	b.memAvailable = func() (int, bool) { return SessionVMMemory + sessionMemoryMargin, true }
	if err := h.Ready(); err != nil {
		t.Errorf("enough memory: %v", err)
	}
	b.memAvailable = func() (int, bool) { return 0, false } // unknown: not refused
	if err := h.Ready(); err != nil {
		t.Errorf("unknown memory: %v", err)
	}
	b.kvm = func() (bool, string) { return false, "no KVM" }
	if err := h.Ready(); err == nil {
		t.Error("ready without KVM")
	}
}
