package guest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

func TestFindPort(t *testing.T) {
	sys := t.TempDir()
	for port, name := range map[string]string{"vport0p0": "org.qemu.guest_agent.0", "vport1p1": "rubi.control\n", "vport1p2": "rubi.control.x"} {
		d := filepath.Join(sys, "class", "virtio-ports", port)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "name"), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := FindPort(sys, "/dev", ControlPortName)
	if err != nil || got != "/dev/vport1p1" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := FindPort(sys, "/dev", "rubi.other"); err == nil {
		t.Error("found a port that isn't there")
	}
}

// QEMU names the port (microvm.ControlPort) and the guest looks for it by name: the two must agree.
func TestControlPortNameIsQEMUs(t *testing.T) {
	if ControlPortName != microvm.ControlPort {
		t.Fatalf("guest %q, QEMU %q", ControlPortName, microvm.ControlPort)
	}
}
