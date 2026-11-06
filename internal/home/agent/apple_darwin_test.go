package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerBinPrefersTheInstalledOne(t *testing.T) {
	old := installedBin
	defer func() { installedBin = old }()
	dir := t.TempDir()
	installedBin = filepath.Join(dir, "container")
	if err := os.WriteFile(installedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "container"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", other)
	if got := ContainerBin(); got != installedBin {
		t.Errorf("got %q", got)
	}
	installedBin = filepath.Join(dir, "missing")
	if got := ContainerBin(); got != filepath.Join(other, "container") {
		t.Errorf("fallback to PATH: %q", got)
	}
}

// The tool's exec name is what the agent-vm shim runs; the platform backend on an Apple silicon Mac is Apple's.
func TestThePlatformBackendOnAMacIsApples(t *testing.T) {
	old := appleSilicon
	defer func() { appleSilicon = old }()
	appleSilicon = func() bool { return true }
	b := PlatformBackend(BackendConfig{})
	if b.Name() != "apple-container" || ExecToolName() != "container" || !b.Available(nil).OK {
		t.Fatalf("%s %q", b.Name(), ExecToolName())
	}
}

// Apple's container tool runs only on Apple silicon, so an Intel Mac has no backend, and says why in one calm sentence.
func TestAnIntelMacHasNoAgents(t *testing.T) {
	old := appleSilicon
	defer func() { appleSilicon = old }()
	appleSilicon = func() bool { return false }
	b := PlatformBackend(BackendConfig{})
	av := b.Available(nil)
	if b.Name() != "none" || av.OK || av.Reason != "agents need a Mac with Apple silicon" {
		t.Fatalf("%s %+v", b.Name(), av)
	}
	if b.Login() != nil || b.Sessions() != nil || b.Provision() != nil {
		t.Fatal("an Intel Mac must not get the parts of a backend")
	}
}

// The hardware check reads the kernel: this test's own Mac is Apple silicon exactly when the kernel says hw.optional.arm64.
func TestAppleSiliconReadsTheHardware(t *testing.T) {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.optional.arm64").Output()
	want := err == nil && strings.TrimSpace(string(out)) == "1"
	if got := appleSilicon(); got != want {
		t.Fatalf("appleSilicon() = %v, sysctl says %v", got, want)
	}
}
