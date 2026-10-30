package main

import (
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home"
)

func TestWaitLimit(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{nil, home.ClaudeWaitLimit},
		{[]string{"--wait-limit", "120"}, 120 * time.Second},
		{[]string{"--wait-limit", "0"}, home.ClaudeWaitLimit},
		{[]string{"--wait-limit", "x"}, home.ClaudeWaitLimit},
		{[]string{"--wait-limit"}, home.ClaudeWaitLimit},
	} {
		if got := waitLimit(tc.args); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.args, got, tc.want)
		}
	}
	if home.ClaudeWaitLimit != 3570*time.Second {
		t.Fatal("the default limit is the example timeout (3600 s) less 30 s")
	}
}
