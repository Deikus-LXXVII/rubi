//go:build !darwin

package agent

import (
	"context"
	"runtime"
	"testing"
)

// Off macOS no container tool is named, so the agent-vm shim would run no command; Windows has no VM backend yet (it is not
// available and says why), Linux has the microvm backend (TestLinuxRunsTheMicroVMBackend).
func TestPlatformBackendOffMacOS(t *testing.T) {
	if ContainerBin() != "" || ExecToolName() != "" {
		t.Errorf("a tool is named: %q %q", ContainerBin(), ExecToolName())
	}
	if runtime.GOOS == "linux" {
		return
	}
	b := PlatformBackend(BackendConfig{})
	av := b.Available(context.Background())
	if b.Name() != "none" || av.OK || av.Reason == "" {
		t.Fatalf("%s %+v", b.Name(), av)
	}
}
