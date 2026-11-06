package agent

import (
	"context"
	"testing"
)

// Linux gets the microvm backend, its setup installs the VM files itself (ToolInstaller), and it has no T3 session host yet.
func TestLinuxRunsTheMicroVMBackend(t *testing.T) {
	b := PlatformBackend(BackendConfig{EnvDir: t.TempDir()})
	if b.Name() != "microvm" {
		t.Fatalf("backend %q", b.Name())
	}
	mb := b.(*MicroVMBackend)
	if mb.exe == "" || mb.files.Version == "" {
		t.Fatalf("exe %q, version %q", mb.exe, mb.files.Version)
	}
	if b.Sessions() != nil {
		t.Error("a session host before T3 runs on microvm")
	}
	mb.landlock, mb.kvm = func() error { return nil }, func() (bool, string) { return true, "" }
	if !b.Available(context.Background()).OK {
		t.Fatal("not available")
	}
	if _, ok := b.Provision().(ToolInstaller); !ok {
		t.Error("the setup can't install the VM files")
	}
}
