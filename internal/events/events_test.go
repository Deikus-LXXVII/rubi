package events

import (
	"testing"
	"time"
)

func ids(list []Event) map[string]bool {
	m := map[string]bool{}
	for _, e := range list {
		m[e.ID] = true
	}
	return m
}

func TestAcksArePerAgent(t *testing.T) {
	s := NewStore()
	e := s.Emit("rubi", "update.available", map[string]any{}, nil)
	if got := s.Ack("Alice", e.ID); got != Acked {
		t.Fatalf("first ack: %s", got)
	}
	if got := s.Ack(" alice ", e.ID); got != AlreadyAcked {
		t.Fatalf("second ack (names compare case-insensitively): %s", got)
	}
	if ids(s.ListFor("alice", false))[e.ID] {
		t.Fatal("event still visible to the agent that acked it")
	}
	if !ids(s.ListFor("Bob", false))[e.ID] {
		t.Fatal("event hidden from an agent that didn't ack it")
	}
	if got := s.Ack("Bob", e.ID); got != Acked {
		t.Fatalf("Bob's ack: %s", got)
	}
	all := s.ListFor("alice", true)
	if len(all) != 1 || !all[0].Acked || all[0].AckedBy != nil {
		t.Fatalf("include_acked view: %+v", all)
	}
	if got := s.Ack("Bob", "evt_nope"); got != NotFound {
		t.Fatalf("unknown id: %s", got)
	}
}

func TestOldAckedFlagMeansAckedByAll(t *testing.T) {
	s := NewStore()
	now := time.Now().UTC()
	s.Restore([]Event{{ID: "evt_old", Integration: "rubi", Type: "x", CreatedAt: now, Acked: true},
		{ID: "evt_new", Integration: "rubi", Type: "x", CreatedAt: now}})
	for _, who := range []string{"a", "b", ""} {
		got := ids(s.ListFor(who, false))
		if got["evt_old"] || !got["evt_new"] {
			t.Fatalf("%q sees %v", who, got)
		}
	}
	if got := s.Ack("a", "evt_old"); got != AlreadyAcked {
		t.Fatalf("ack of a migrated event: %s", got)
	}
	if len(s.List(false)) != 1 {
		t.Fatal("List(false) keeps events acked by all")
	}
}

func TestAckedByAllSurvivesRestore(t *testing.T) {
	s := NewStore()
	e := s.Emit("rubi", "x", nil, nil)
	s.Ack("a", e.ID)
	s.AckAll(e.ID)
	s.Ack("b", s.Emit("rubi", "y", nil, nil).ID)
	saved := s.List(true)
	r := NewStore()
	r.Restore(saved)
	if ids(r.ListFor("zed", false))[e.ID] {
		t.Fatal("event acked by all resurfaced")
	}
	if len(r.List(false)) != 1 {
		t.Fatalf("pending after restore: %d", len(r.List(false)))
	}
}

func TestPrune(t *testing.T) {
	s := NewStore()
	old := time.Now().UTC().Add(-2 * time.Hour)
	s.Restore([]Event{
		{ID: "evt_all", CreatedAt: old, AckedBy: map[string]time.Time{AckedAll: old}},
		{ID: "evt_one", CreatedAt: old, AckedBy: map[string]time.Time{"a": old}},
		{ID: "evt_day", CreatedAt: time.Now().UTC().Add(-25 * time.Hour)},
	})
	s.Emit("rubi", "trigger", nil, nil) // prunes on emit
	got := ids(s.List(true))
	if got["evt_all"] || got["evt_day"] || !got["evt_one"] {
		t.Fatalf("after prune: %v", got)
	}
	if !ids(s.ListFor("b", false))["evt_one"] {
		t.Fatal("an event one agent acked was pruned before the other saw it")
	}
}

func TestNotStored(t *testing.T) {
	for id, want := range map[string]bool{"evt_access_abcdefgh": true, "evt_code_abcdefgh": true, "evt_locked_abcdefgh": true,
		"evt_stopping_abcdefgh": true, "evt_code_abcdefg": false, "evt_abcdefghijkl": false, "": false} {
		if NotStored(id) != want {
			t.Errorf("NotStored(%q) != %v", id, want)
		}
	}
}
