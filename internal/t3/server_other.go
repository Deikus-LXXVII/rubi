//go:build !unix

package t3

import (
	"os/exec"
	"time"
)

func own(*exec.Cmd) {}

func stop(cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
		return
	default:
	}
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func ours(pid, root int) bool { return pid == root }

func stopAdopted(int, <-chan struct{}, func() bool) {}

func sameUser(int) bool { return false }
