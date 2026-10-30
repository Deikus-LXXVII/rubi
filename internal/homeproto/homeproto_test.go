package homeproto

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
)

// An operation that never returns (a read stuck on a hanging disk) still gets an answer by the deadline:
// an error the caller can show, not silence until its own timeout.
func TestHandleRPCAnswersStuckOperationByDeadline(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	stuck := make(chan struct{})
	defer close(stuck)
	srv := &Server{Key: key, Handle: func(context.Context, Envelope) (any, error) {
		<-stuck // ignores ctx, like a blocked open(2)
		return "late", nil
	}}
	env := Envelope{Op: "claude-sessions.list", TS: time.Now().Unix(), RID: RandomToken()}
	plain, _ := json.Marshal(env)
	req, respKey, err := e2e.Client{InstanceKey: key.PublicKey()}.Seal(Path, plain)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	status, resp := srv.HandleRPC(ctx, body)
	if took := time.Since(start); status != http.StatusOK || took > 2*time.Second {
		t.Fatalf("status %d after %s", status, took)
	}
	var sealed e2e.Response
	if err := json.Unmarshal(resp, &sealed); err != nil {
		t.Fatal(err)
	}
	opened, err := e2e.OpenResponse(respKey, env.RID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var r reply
	if err := json.Unmarshal(opened, &r); err != nil || r.OK || !strings.Contains(r.Error, "in time") {
		t.Fatalf("reply %s (%v)", opened, err)
	}
}

// An operation stuck past its deadline holds one goroutine at most: later calls of it fail at once and
// start nothing. Other operations aren't affected.
func TestStuckOperationIsCappedAtOneHandler(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	srv := &Server{Handle: func(_ context.Context, env Envelope) (any, error) {
		if env.Op == "claude-sessions.list" {
			<-stuck
		}
		return "ok", nil
	}}
	call := func(op string, d time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		_, err := srv.handle(ctx, Envelope{Op: op})
		return err
	}
	before := runtime.NumGoroutine()
	if err := call("claude-sessions.list", 20*time.Millisecond); err == nil {
		t.Fatal("no error for the stuck call")
	}
	for i := 0; i < 100; i++ {
		start := time.Now()
		if err := call("claude-sessions.list", time.Second); err == nil || time.Since(start) > 200*time.Millisecond {
			t.Fatalf("call %d: %v after %s", i, err, time.Since(start))
		}
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+1 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
	if err := call("other", time.Second); err != nil {
		t.Fatalf("another operation: %v", err)
	}
}

// A burst of calls of a stuck operation can't start more than maxOpHandlers handlers, and once one is
// stuck nothing new starts.
func TestBurstOfStuckCallsIsCapped(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	var started atomic.Int32
	srv := &Server{Handle: func(context.Context, Envelope) (any, error) {
		started.Add(1)
		<-stuck
		return nil, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, err := srv.handle(ctx, Envelope{Op: "claude-sessions.list"}); err == nil {
				t.Error("a stuck call succeeded")
			}
		}()
	}
	wg.Wait()
	if n := started.Load(); n < 1 || n > maxOpHandlers {
		t.Fatalf("%d handlers started, want 1..%d", n, maxOpHandlers)
	}
	before := started.Load()
	if _, err := srv.handle(context.Background(), Envelope{Op: "claude-sessions.list"}); err == nil || started.Load() != before {
		t.Fatalf("a call after the burst started a handler (%v)", err)
	}
}

// A handler that panics ends its call with an error and frees its place.
func TestPanickingHandlerFreesItsPlace(t *testing.T) {
	srv := &Server{Handle: func(context.Context, Envelope) (any, error) { panic("boom") }}
	for i := 0; i < 3*maxOpHandlers; i++ {
		if _, err := srv.handle(context.Background(), Envelope{Op: "x"}); err == nil || strings.Contains(err.Error(), "still busy") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
