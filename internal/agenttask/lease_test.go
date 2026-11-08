package agenttask

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTaskLeaseStopDrainsUnknownLaunchAndFencesLateStarts(t *testing.T) {
	st, w := taskFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := st.Admit(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Admit(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer b.Close()
	if _, err = st.Outcome(ctx, w.ID, "", Stopped); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Context().Done():
	case <-ctx.Done():
		t.Fatal("launcher did not learn fence")
	}
	if _, err = st.Admit(ctx, w.ID); err == nil {
		t.Fatal("late native launch admitted")
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	if err = st.Quiesce(short, w.ID); err == nil {
		t.Fatal("stop acknowledged before both providers exited")
	}
	stop()
	a.Close()
	b.Close()
	if err = st.Quiesce(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if err = (Store{Home: st.Home}).Quiesce(ctx, w.ID); err != nil {
		t.Fatal("restart lost cleanup receipts", err)
	}
}
func TestTaskAbandonedLauncherIsNeverACleanupReceipt(t *testing.T) {
	st, w := taskFixture(t)
	gate, dir, err := st.leaseGate(context.Background(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	gate.Close()
	f, err := os.CreateTemp(dir, "run-")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("live\n")
	f.Close() // simulate a supervisor crash, no completion receipt
	if _, err = st.Outcome(context.Background(), w.ID, "", Stopped); err != nil {
		t.Fatal(err)
	}
	if err = (Store{Home: st.Home}).Quiesce(context.Background(), w.ID); err == nil || !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatal("abandoned process declared stopped", err)
	}
}
func TestTaskPublicationWaitsForProviderExitBeforeSnapshot(t *testing.T) {
	st, w := taskFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := st.Admit(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	// A sealed mandate cancels a provider, but cancellation is not exit.
	if err = st.With(ctx, w.ID, func(tx *Tx) error { tx.State.Phase = Publishing; return tx.Save() }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Context().Done():
	case <-ctx.Done():
		t.Fatal("provider not cancelled")
	}
	short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	if err = st.Quiesce(short, w.ID); err == nil {
		t.Fatal("snapshot permitted before provider exit")
	}
	stop()
	lease.Close()
	if err = st.Quiesce(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
}
func TestTaskLeaseUnknownTaskAndMutableQuiescenceRefuse(t *testing.T) {
	st, w := taskFixture(t)
	if _, err := st.Admit(context.Background(), "task_012345678901234567890123"); err == nil {
		t.Fatal("missing task admitted")
	}
	if err := st.Quiesce(context.Background(), w.ID); err == nil {
		t.Fatal("unfenced quiescence accepted")
	}
}
