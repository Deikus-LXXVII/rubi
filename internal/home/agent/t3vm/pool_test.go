package t3vm

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestClampedArgvSessionID(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--model", "m", "--session-id=abc"}, "abc"},
		{[]string{"--session-id", "def", "--model", "m"}, "def"},
		{[]string{"--resume", "ghi"}, "ghi"},
		{[]string{"--resume=jkl"}, "jkl"},
		{[]string{"--model", "m"}, ""},
		{[]string{"--session-id"}, ""},
	} {
		if got := (ClampedArgv{args: tc.args}).SessionID(); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.args, got, tc.want)
		}
	}
}

// An entry that was stopped while it was still in the pool (between a stop and its drop) is never handed out: the next
// acquire starts a new VM.
func TestPoolNeverHandsOutAStoppedVM(t *testing.T) {
	var p vmPool
	starts := 0
	start := func(context.Context, *roleVM) error { starts++; return nil }
	v1, rel1, err := p.acquire(context.Background(), context.Background(), "r", start)
	if err != nil {
		t.Fatal(err)
	}
	rel1()
	v1.stop() // stopped, still the pool's entry
	v2, rel2, err := p.acquire(context.Background(), context.Background(), "r", start)
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	if v2 == v1 || starts != 2 {
		t.Errorf("handed out the stopped VM (starts %d)", starts)
	}
	p.stopAll()
}

func TestSessionIDPrefersANewID(t *testing.T) {
	if got := (ClampedArgv{args: []string{"--resume", "old", "--fork-session", "--session-id", "new"}}).SessionID(); got != "new" {
		t.Errorf("got %q", got)
	}
}

// Acquires at once share one start; a caller that leaves while the VM starts doesn't fail the others.
func TestPoolSharesOneStartAndOutlivesALeavingCaller(t *testing.T) {
	var p vmPool
	var mu sync.Mutex
	starts := 0
	gate := make(chan struct{})
	start := func(ctx context.Context, _ *roleVM) error {
		mu.Lock()
		starts++
		mu.Unlock()
		select {
		case <-gate:
			return nil
		case <-ctx.Done(): // the start's own context: a caller leaving must not end it
			return ctx.Err()
		}
	}
	leaving, leave := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, _, err := p.acquire(leaving, context.Background(), "r", start); first <- err }()
	for { // the leaving caller is the one that starts the VM
		mu.Lock()
		n := starts
		mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	second := make(chan *roleVM, 1)
	go func() {
		v, rel, err := p.acquire(context.Background(), context.Background(), "r", start)
		if err != nil {
			t.Error(err)
		}
		defer rel()
		second <- v
	}()
	time.Sleep(50 * time.Millisecond)
	leave()
	if err := <-first; err == nil {
		t.Error("the leaving caller got a VM")
	}
	close(gate)
	v := <-second
	mu.Lock()
	n := starts
	mu.Unlock()
	if v == nil || n != 1 {
		t.Errorf("starts %d", n)
	}
	v.mu.Lock()
	stopped := v.stopped
	v.mu.Unlock()
	if stopped {
		t.Error("the VM was stopped because a caller left")
	}
	p.stopAll()
}

// A VM whose only caller left before it was up stops when idle.
func TestPoolStopsAStartNobodyStayedFor(t *testing.T) {
	old := roleVMIdle
	roleVMIdle = 20 * time.Millisecond
	t.Cleanup(func() { roleVMIdle = old })
	var p vmPool
	gate := make(chan struct{})
	got := make(chan *roleVM, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _, _ = p.acquire(ctx, context.Background(), "r", func(_ context.Context, rv *roleVM) error { got <- rv; <-gate; return nil })
		close(done)
	}()
	v := <-got
	cancel()
	<-done
	close(gate)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		v.mu.Lock()
		stopped := v.stopped
		v.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a VM nobody stayed for runs on")
		}
	}
}

// The idle timer never stops a VM a session holds: acquire and release against a tiny idle time, over and over.
func TestPoolIdleNeverStopsAHeldVM(t *testing.T) {
	old := roleVMIdle
	roleVMIdle = time.Microsecond
	t.Cleanup(func() { roleVMIdle = old })
	var p vmPool
	start := func(context.Context, *roleVM) error { return nil }
	for i := 0; i < 2000; i++ {
		v, rel, err := p.acquire(context.Background(), context.Background(), "r", start)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i%3) * time.Microsecond)
		v.mu.Lock()
		stopped := v.stopped
		v.mu.Unlock()
		if stopped {
			t.Fatalf("round %d: the VM was stopped while a session held it", i)
		}
		rel()
	}
	p.stopAll()
}

// A teardown step added after the VM was stopped (its pipe gave up while it started) runs at once.
func TestRoleVMAddAfterStopRunsAtOnce(t *testing.T) {
	v := &roleVM{ready: make(chan struct{})}
	v.ctx, v.cancel = context.WithCancel(context.Background())
	ran := 0
	v.add(func() { ran++ })
	v.stop()
	if ran != 1 {
		t.Fatalf("teardown ran %d times", ran)
	}
	v.add(func() { ran++ })
	if ran != 2 {
		t.Error("a step added after the stop never ran")
	}
}
