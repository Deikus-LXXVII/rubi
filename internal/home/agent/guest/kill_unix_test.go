//go:build unix

package guest

import (
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// groupRig is a rig whose processes run in a group of their own, killed as a group (as on Linux).
func groupRig(t *testing.T) *rig {
	r := newRig(t)
	r.srv.Prepare = func(cmd *exec.Cmd, _ ExecRequest) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
	r.srv.Kill = func(cmd *exec.Cmd) { r.killed.Add(1); _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return r
}

// alive says whether pid is a running process (not gone, and on Linux not a zombie waiting for its parent).
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && len(out) > 0 && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// A process that reads none of its stdin, with Home's writes stuck behind it, is still killed when Home closes it.
func TestCloseKillsAProcessThatIgnoresStdin(t *testing.T) {
	r := groupRig(t)
	p, err := r.c.Exec(ctx5(t), sh("echo $$; exec sleep 30"), nil)
	if err != nil {
		t.Fatal(err)
	}
	line := make([]byte, 32)
	n, _ := p.Read(line)
	pid, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatalf("%q", line[:n])
	}
	go func() { _, _ = p.Write(make([]byte, 1<<20)) }() // far more than the pipe and the stream's credit hold
	time.Sleep(300 * time.Millisecond)
	_ = p.Close()
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Fatalf("process %d still runs after Close", pid)
	}
}

// A descendant that keeps the output pipes after the leader exits can't hold the stream: the exit status comes after
// the grace.
func TestLeaderExitEndsTheStreamDespiteADescendant(t *testing.T) {
	old := outputGrace
	outputGrace = 300 * time.Millisecond
	t.Cleanup(func() { outputGrace = old })
	r := groupRig(t)
	start := time.Now()
	p, err := r.c.Exec(ctx5(t), sh("sleep 30 & echo $!"), nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p)
	code, err := p.Wait()
	if code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the exit status came after %v, held by the descendant", d)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}
