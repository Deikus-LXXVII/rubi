package core

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

func ack(t *testing.T, c *Core, agent, id string) string {
	t.Helper()
	r, err := c.AckFor(agent, c.agentCodeFor(agent), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAckReasonsPerAgent(t *testing.T) {
	c := targetCore(t)
	broadcast := c.Events.EmitFor("", true, "demo-mail", "ping", map[string]any{}, nil)
	toMail := c.Events.EmitFor("Mail", true, "demo-mail", "private", map[string]any{}, nil)

	if r := ack(t, c, "Main", broadcast.ID); r != "acked" {
		t.Fatalf("Main: %s", r)
	}
	if r := ack(t, c, "Mail", broadcast.ID); r != "acked" {
		t.Fatalf("Mail (second Bot on the same broadcast): %s", r)
	}
	if r := ack(t, c, "Mail", broadcast.ID); r != "already_acked" {
		t.Fatalf("repeat: %s", r)
	}
	if r := ack(t, c, "Main", toMail.ID); r != "not_visible" {
		t.Fatalf("addressed to another Bot: %s", r)
	}
	if r := ack(t, c, "Main", "evt_gone"); r != "unknown_or_expired" {
		t.Fatalf("unknown: %s", r)
	}
	if r := ack(t, c, "Main", "evt_access_12345678"); r != "not_stored" {
		t.Fatalf("webhook-only id: %s", r)
	}
	if r := ack(t, c, "Mail", toMail.ID); r != "acked" {
		t.Fatalf("Mail's own: %s", r)
	}
	// The addressed event has every addressee's ack: settled (prunable early). The broadcast still waits
	// for Half, a subscribed Bot that hasn't acked.
	pending := c.Events.List(false)
	if len(pending) != 1 || pending[0].ID != broadcast.ID {
		t.Fatalf("pending after acks: %+v", pending)
	}
}

func TestAckHidesOnlyForTheAckingAgent(t *testing.T) {
	c := targetCore(t)
	e := c.Events.EmitFor("", true, "demo-mail", "ping", map[string]any{}, nil)
	ack(t, c, "Main", e.ID)
	has := func(agent string) bool {
		evs, err := c.EventsFor(agent, c.agentCodeFor(agent), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range evs {
			if x.ID == e.ID {
				return true
			}
		}
		return false
	}
	if has("Main") || !has("Mail") {
		t.Fatalf("visibility after Main's ack: Main %v, Mail %v", has("Main"), has("Mail"))
	}
	if c.WaitingFor("Main") != 0 || c.WaitingFor("Mail") != 1 {
		t.Fatalf("waiting: Main %d, Mail %d", c.WaitingFor("Main"), c.WaitingFor("Mail"))
	}
}

func TestOldSavedAckedEventStaysHidden(t *testing.T) {
	c := targetCore(t)
	c.Events.Restore([]events.Event{{ID: "evt_legacy0000001", Integration: "demo-mail", Type: "ping", CreatedAt: time.Now().UTC(), Acked: true}})
	for _, a := range []string{"Main", "Mail"} {
		evs, _ := c.EventsFor(a, c.agentCodeFor(a), false)
		if len(evs) != 0 {
			t.Fatalf("%s sees a migrated acked event", a)
		}
	}
}

// access.granted is never stored, so its message must not tell the Bot to ack it.
func TestAccessGrantedNextStepHasNoAck(t *testing.T) {
	c := targetCore(t)
	got := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		got <- m
	}))
	defer srv.Close()
	ev := events.Event{ID: "evt_access_abcd1234", Integration: "rubi", Type: "access.granted", CreatedAt: time.Now().UTC(),
		Data: map[string]any{"next_step": "Continue your task."}}
	c.deliverTo(vault.Agent{Name: "Main", URL: srv.URL}, ev)
	m := <-got
	next, _ := m["next_step"].(string)
	if next == "" || strings.Contains(next, "rubi_ack") || strings.Contains(next, "rubi_events") {
		t.Fatalf("next_step: %q", next)
	}
	// An event without its own next_step still gets one that doesn't mention ack.
	ev.Data = map[string]any{}
	c.deliverTo(vault.Agent{Name: "Main", URL: srv.URL}, ev)
	if n, _ := (<-got)["next_step"].(string); strings.Contains(n, "rubi_ack") || n == "" {
		t.Fatalf("generic next_step: %q", n)
	}
}
