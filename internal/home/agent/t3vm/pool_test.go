package t3vm

import (
	"context"
	"testing"
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
	start := func(*roleVM) error { starts++; return nil }
	v1, rel1, err := p.acquire(context.Background(), "r", start, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	rel1()
	v1.stop(func(string, ...any) {}) // stopped, still the pool's entry
	v2, rel2, err := p.acquire(context.Background(), "r", start, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	if v2 == v1 || starts != 2 {
		t.Errorf("handed out the stopped VM (starts %d)", starts)
	}
	p.stopAll(func(string, ...any) {})
}
