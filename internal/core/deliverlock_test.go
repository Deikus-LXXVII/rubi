package core

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/events"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Locking stops the webhook deliveries in flight and waits for them: one whose request hangs and one waiting to try again
// both end at once, and nothing of theirs comes after the lock. (Before, a delivery went on for its retries, up to ten
// minutes, and wrote its audit lines after the lock: the tests' folders could not be removed, "directory not empty".)
func TestLockStopsTheDeliveriesInFlight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request)
	}{
		// the body is read first: only then does the server notice the client hang up and end the request's context
		{"hanging request", func(w http.ResponseWriter, r *http.Request) { _, _ = io.ReadAll(r.Body); <-r.Context().Done() }},
		{"waiting to retry", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusInternalServerError) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := botCore(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.handler(w, r)
			}))
			t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() }) // the first ends a still hanging handler
			c.goDeliver(vault.Agent{Name: "Main", URL: srv.URL}, events.Event{ID: "evt_lock_1", Integration: "rubi", Type: "ping", CreatedAt: time.Now().UTC()})
			for deadline := time.Now().Add(5 * time.Second); calls.Load() == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the delivery never reached the webhook")
				}
			}
			time.Sleep(50 * time.Millisecond) // the failing one is now in its 5 s wait before the next try
			start := time.Now()
			c.Lock()
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("Lock took %s: it waited out the delivery instead of stopping it", d)
			}
			// stop has waited for the delivery: it has ended, and it makes no request any more
			if n := c.deliveries.active.Load(); n != 0 {
				t.Fatalf("%d deliveries still in flight after the lock", n)
			}
			n := calls.Load()
			time.Sleep(200 * time.Millisecond)
			if calls.Load() != n {
				t.Error("a delivery went on after the lock")
			}
		})
	}
}

// After a lock, the next unlock's deliveries get a context of their own (stop does not leave a cancelled one behind).
func TestDeliveriesAfterAStopHaveALiveContext(t *testing.T) {
	var d deliveries
	ctx := d.start()
	d.done()
	d.stop()
	if ctx.Err() == nil {
		t.Fatal("the first delivery's context was not cancelled by stop")
	}
	next := d.start()
	defer d.done()
	if next.Err() != nil {
		t.Fatal("a delivery after the stop got a cancelled context")
	}
}
