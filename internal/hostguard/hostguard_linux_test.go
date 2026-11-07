package hostguard

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestSetNonDumpable leaves this test binary non-dumpable; later tests in the package must not need to read their own
// /proc entries as another user.
func TestSetNonDumpable(t *testing.T) {
	if err := SetNonDumpable(); err != nil {
		t.Fatal(err)
	}
	d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil || d != 0 {
		t.Fatalf("dumpable = %d, %v; want 0", d, err)
	}
	h, err := Probe()
	if err != nil {
		t.Fatal(err)
	}
	if h.Dumpable {
		t.Error("Probe says the process is dumpable after SetNonDumpable")
	}
}
