package agenttask

import (
	"context"
	"testing"
)

func TestTaskTerminalDeliveryReceiptSurvivesRestartWithoutChangingOutcome(t *testing.T) {
	st, w := taskFixture(t)
	ctx := context.Background()
	if err := st.AcknowledgeResult(ctx, w.ID, w.ID+":terminal"); err == nil {
		t.Fatal("running task acknowledged")
	}
	state, err := st.Outcome(ctx, w.ID, "thread", Stopped)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AcknowledgeResult(ctx, w.ID, "another-task:terminal"); err == nil {
		t.Fatal("wrong event acknowledged")
	}
	if st.ResultAcknowledged(ctx, w.ID) {
		t.Fatal("undelivered task marked delivered")
	}
	if err = st.AcknowledgeResult(ctx, w.ID, state.EventID()); err != nil {
		t.Fatal(err)
	}
	restarted := Store{Home: st.Home}
	if !restarted.ResultAcknowledged(ctx, w.ID) {
		t.Fatal("delivery lost on restart")
	}
	if err = restarted.AcknowledgeResult(ctx, w.ID, state.EventID()); err != nil {
		t.Fatal("ack not idempotent", err)
	}
	after, err := restarted.Get(ctx, w.ID)
	if err != nil || after.Revision != state.Revision || after.TerminalAt != state.TerminalAt {
		t.Fatal("delivery changed terminal outcome")
	}
}
