package core

import (
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

func pendingEvents(t *testing.T, c *Core) []events.Event {
	t.Helper()
	var kept []events.Event
	if err := c.Vault.View(func(d *vault.Data) error { kept = d.PendingEvents; return nil }); err != nil {
		t.Fatal(err)
	}
	return kept
}

// Locking keeps the unreported events, then stops the plugins and clears the list before the vault closes. A save that
// runs in between (an event's delivery) must not write the cleared list over what was kept: the event would be lost
// across the lock (seen as a flaky TestHooks in internal/mcpserver under load).
func TestEventsKeptForTheLockSurviveALateSave(t *testing.T) {
	c := targetCore(t)
	e := c.Events.EmitQuiet("demo-mail", "ping", map[string]any{}, nil)
	c.beforeLock("")
	c.Events.Clear() // LockWith's next steps, before the vault is closed
	c.saveEvents()   // a delivery's save in that gap
	if kept := pendingEvents(t, c); len(kept) != 1 || kept[0].ID != e.ID {
		t.Fatalf("kept %+v", kept)
	}
	// after the unlock, events are saved again
	BacklogSettle = 0
	c.afterUnlock()
	time.Sleep(50 * time.Millisecond) // the backlog notice's goroutine
	later := c.Events.EmitQuiet("demo-mail", "later", map[string]any{}, nil)
	c.saveEvents()
	found := false
	for _, k := range pendingEvents(t, c) {
		found = found || k.ID == later.ID
	}
	if !found {
		t.Fatal("an event after the unlock was not saved")
	}
}
