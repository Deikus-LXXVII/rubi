package agentsandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeCancellationReapsOuterShellAndBackgroundWriter(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", `trap '' TERM; (while :; do printf x >> "$1"; sleep .02; done) & wait`, "fixture", marker)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	done := make(chan error, 1)
	go func() { done <- runConfined(ctx, cmd, false) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, e := os.ReadFile(marker); e == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("fixture writer did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not reap")
	}
	before, _ := os.ReadFile(marker)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.ReadFile(marker)
	if !bytes.Equal(before, after) {
		t.Fatal("background writer survived runtime cancellation")
	}
}
func TestRuntimeNormalExitDoesNotLeaveBackgroundPipeOwners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.Command("/bin/sh", "-c", `sleep 60 & exit 0`)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := runConfined(ctx, cmd, false); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("background pipe owner delayed natural exit")
	}
}

func TestOwnedNamespaceReapsDetachedDescendantBeforeReturning(t *testing.T) {
	root := t.TempDir()
	heartbeat := filepath.Join(root, "heartbeat")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `setsid /bin/sh -c 'while :; do echo x >> "$1"; sleep 0.01; done' sh "$1" & wait`, "sh", heartbeat)
	cmd.Cancel = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- runConfined(ctx, cmd, true) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, e := os.ReadFile(heartbeat); e == nil && len(b) > 0 {
			break
		}
		select {
		case err := <-done:
			if os.Getenv("RUBI_TEST_PID_NAMESPACE") != "1" && (errors.Is(err, syscall.EPERM) || strings.Contains(stderr.String(), "unshare failed: Operation not permitted")) {
				t.Skip("runner disallows user/mount namespaces; production tasks fail closed")
			}
			t.Fatal("owned namespace unavailable", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("detached helper did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("namespace stop hung")
	}
	before, _ := os.ReadFile(heartbeat)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.ReadFile(heartbeat)
	if !bytes.Equal(before, after) {
		t.Fatal("detached process survived confirmed namespace reaping")
	}
}

func TestNamespaceGuardianFixture(t *testing.T) {
	root := os.Getenv("RUBI_NAMESPACE_GUARDIAN_FIXTURE")
	if root == "" {
		t.Skip("subprocess fixture")
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", `setsid /bin/sh -c 'while :; do echo x >> "$1"; sleep 0.01; done' sh "$1" & wait`, "sh", filepath.Join(root, "heartbeat"))
	cmd.Cancel = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := runConfined(context.Background(), cmd, true); err != nil {
		t.Fatalf("fixture: %v: %s", err, stderr.String())
	}
}
func TestOwnedNamespaceGuardianCrashKillsDetachedProcess(t *testing.T) {
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestNamespaceGuardianFixture$")
	cmd.Env = append(os.Environ(), "RUBI_NAMESPACE_GUARDIAN_FIXTURE="+root)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var childErr error
	go func() { childErr = cmd.Wait(); close(done) }()
	defer func() { _ = cmd.Process.Kill(); <-done }()
	heartbeat := filepath.Join(root, "heartbeat")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, e := os.ReadFile(heartbeat); e == nil && len(b) > 0 {
			break
		}
		select {
		case <-done:
			if os.Getenv("RUBI_TEST_PID_NAMESPACE") != "1" && strings.Contains(strings.ToLower(stderr.String()), "operation not permitted") {
				t.Skip("runner disallows namespaces; production tasks refuse")
			}
			t.Fatal("guardian fixture exited before launch", childErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("guardian fixture failed to start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// No successful lease receipt is synthesized on this crash. Assert only that
	// parent-death tears down the actual unknown runtime and detached child.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		before, _ := os.ReadFile(heartbeat)
		time.Sleep(100 * time.Millisecond)
		after, _ := os.ReadFile(heartbeat)
		if bytes.Equal(before, after) {
			return
		}
	}
	t.Fatal("guardian crash left detached execution running")
}
