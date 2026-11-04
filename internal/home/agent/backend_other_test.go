//go:build !darwin

package agent

import (
	"context"
	"testing"
)

// On Linux and Windows the platform has no VM backend yet: it is not available and says why, no tool is named, and the
// agent-vm shim would run no command.
func TestPlatformBackendIsNoneWhereThereIsNoBackend(t *testing.T) {
	b := PlatformBackend(BackendConfig{})
	av := b.Available(context.Background())
	if b.Name() != "none" || av.OK || av.Reason == "" {
		t.Fatalf("%s %+v", b.Name(), av)
	}
	if ContainerBin() != "" || ExecToolName() != "" {
		t.Errorf("a tool is named: %q %q", ContainerBin(), ExecToolName())
	}
}
