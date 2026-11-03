package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// `rubi-home agent-vm` is what T3 runs as a role's claude: with no Home to ask it must say so and fail, and with bad
// arguments it must explain; it never starts anything.
func TestAgentVMSubcommandFailsPlainlyWithoutHome(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rubi-home")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	run := func(args ...string) (int, string) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "RUBI_HOME_DIR="+t.TempDir())
		var stderr strings.Builder
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, stderr.String()
	}
	if code, msg := run("agent-vm", "--role", "r_aaaaaa01", "--", "--verbose"); code != 1 || !strings.Contains(msg, "isn't reachable") {
		t.Errorf("no Home: exit %d, %q", code, msg)
	}
	if code, msg := run("agent-vm", "--", "--verbose"); code != 2 || !strings.Contains(msg, "usage") {
		t.Errorf("no role: exit %d, %q", code, msg)
	}
}
