package home

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Where the computer has no VM backend, the T3 session server serves nothing: it returns at once and makes no socket.
func TestSessionServerStartsOnlyWhereThereIsABackend(t *testing.T) {
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.T3Port, c.T3BaseDir = 4410, t.TempDir() }); err != nil {
		t.Fatal(err)
	}
	h.backendOverride = func() agent.VMBackend { return agent.NoBackend("agents don't run on Linux yet") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.ServeAgentVM(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session server serves on a computer with no backend")
	}
	if _, err := os.Stat(AgentVMSocketPath(h.Dir)); err == nil {
		t.Error("a session socket was made")
	}
	if h.sessionServer() != nil {
		t.Error("a session server is registered")
	}
}

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

// A backend with no session host gets agents but not T3, and the session server serves nothing on it (it would have no
// VMs to make).
func TestT3NeedsTheBackendsSessionHost(t *testing.T) {
	be := agentsOnlyBackend{agent.NoBackend("unused")}
	got := ComputeFeatures(context.Background(), FeatureEnv{GOOS: "linux", Backend: be})
	if !got[FeatureAgent].Available || got[FeatureT3].Available || got[FeatureT3].Reason != noSessionHost {
		t.Fatalf("agent %+v, t3 %+v", got[FeatureAgent], got[FeatureT3])
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.T3Port, c.T3BaseDir = 4410, t.TempDir() }); err != nil {
		t.Fatal(err)
	}
	h.backendOverride = func() agent.VMBackend { return be }
	done := make(chan error, 1)
	go func() { done <- h.ServeAgentVM(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session server serves with no session host")
	}
	if _, err := os.Stat(AgentVMSocketPath(h.Dir)); err == nil {
		t.Error("a session socket was made")
	}
}

// streamedBackend runs agents and T3 sessions in VMs Home runs itself (the Linux microvm): no SessionHost, but streamed
// sessions.
type streamedBackend struct{ agentsOnlyBackend }

func (streamedBackend) StreamSessions() agent.StreamSessionHost { return nil }

func TestT3IsAvailableWithStreamedSessions(t *testing.T) {
	be := streamedBackend{agentsOnlyBackend{agent.NoBackend("unused")}}
	got := ComputeFeatures(context.Background(), FeatureEnv{GOOS: "linux", Backend: be})
	if !got[FeatureAgent].Available || !got[FeatureT3].Available {
		t.Fatalf("agent %+v, t3 %+v", got[FeatureAgent], got[FeatureT3])
	}
}
