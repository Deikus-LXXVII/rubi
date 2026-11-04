package agent

import (
	"os"
	"path/filepath"
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

// The tool's exec name is what the agent-vm shim runs; the platform backend on a Mac is Apple's.
func TestThePlatformBackendOnAMacIsApples(t *testing.T) {
	b := PlatformBackend(BackendConfig{})
	if b.Name() != "apple-container" || ExecToolName() != "container" || !b.Available(nil).OK {
		t.Fatalf("%s %q", b.Name(), ExecToolName())
	}
}
