package home

import (
	"context"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// The agent manager makes its VMs through the backend: one that is not there makes VMs that never start, and say so.
func TestAgentVMsComeFromTheBackend(t *testing.T) {
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.backendOverride = func() agent.VMBackend { return agent.NoBackend("agents don't run on Windows yet") }
	if av := h.backend().Available(context.Background()); av.OK || av.Reason != "agents don't run on Windows yet" {
		t.Fatalf("%+v", av)
	}
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: tokenHash("tok")}} }); err != nil {
		t.Fatal(err)
	}
	// the setup wizard asks the same backend
	st := h.Setup.Status(context.Background())
	if st.Steps[0].State != "failed" {
		t.Fatalf("the wizard on a computer with no backend: %+v", st.Steps[0])
	}
}

// agentsOnlyBackend runs agents but has no host for T3's sessions yet, like the Linux microvm backend.
type agentsOnlyBackend struct{ agent.VMBackend }

func (agentsOnlyBackend) Available(context.Context) agent.Availability {
	return agent.Availability{OK: true}
}
func (agentsOnlyBackend) Sessions() agent.SessionHost { return nil }
