package main

import (
	"os"
	"path/filepath"
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

func TestInstallHookDryRunAndUnsafePath(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "rubi-home")
	// A user-owned directory is refused even for a dry run (the hook binary must live where only root writes).
	if code := runInstallHook([]string{"--dry-run", "--to", dst}); code == 0 && os.Getuid() != 0 {
		t.Fatal("a user-owned directory was accepted")
	}
	if code := runInstallHook([]string{"--dry-run", "--to", dst + "'x"}); code == 0 {
		t.Fatal("an unsafe path was accepted")
	}
}

func TestHookPath(t *testing.T) {
	t.Setenv("RUBI_HOOK_BINARY", "/tmp/env-hook")
	if got := hookPath(nil); got != "/tmp/env-hook" {
		t.Fatal(got)
	}
	if got := hookPath([]string{"--hook", "/tmp/flag-hook"}); got != "/tmp/flag-hook" {
		t.Fatal(got)
	}
}
