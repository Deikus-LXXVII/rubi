package microvm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Some virtiofsd versions make their socket's file well before they listen; QEMU connects once and fails in that gap.
// The launcher starts QEMU only once the file server listens (/proc/net/unix).
func TestLaunchWaitsForTheFileServersToListen(t *testing.T) {
	r := newLaunchRig(t)
	r.qemuMode(t, "exit0")
	for _, s := range r.spec.Shares {
		_ = os.WriteFile(filepath.Join(s.Source, ".fake"), []byte("slow-listen"), 0o600)
	}
	vm, err := r.start(t)
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, vm, 10*time.Second)
	if err := vm.Err(); err != nil {
		t.Errorf("ended with %v", err)
	}
}

func TestListeningReadsProcNetUnix(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "s.sock")
	if listening(p) {
		t.Fatal("nothing there")
	}
	ln := bindThenListen(p, 0)
	if !listening(p) {
		t.Error("a listening socket not seen")
	}
	ln.Close()
}
