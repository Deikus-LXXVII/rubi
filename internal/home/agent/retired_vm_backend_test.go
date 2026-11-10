package agent

import (
	"context"
	"testing"
)

func TestPlatformCannotStartRetiredVMBackends(t *testing.T) {
	b := PlatformBackend(BackendConfig{Bin: "must-not-execute"})
	if b.Available(context.Background()).OK || b.Name() != "none" || b.Login() != nil || b.Sessions() != nil || b.Provision() != nil {
		t.Fatal("production factory exposed a retired VM backend")
	}
	if _, e := b.New(Project{}).Start(context.Background(), StartSpec{}); e == nil {
		t.Fatal("retired backend started a VM")
	}
}
