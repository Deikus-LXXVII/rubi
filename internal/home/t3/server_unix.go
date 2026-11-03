//go:build unix

package t3

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// own puts the server in its own process group, so Stop reaches its children and nothing else.
func own(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// stop ends the group the command leads: a polite signal first, then a kill. If the leader has already been
// reaped nothing is signalled. The wait is bounded.
func stop(cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
		return
	default:
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	// Children may outlive the leader (the leader was alive when we signalled, so its id still names our group).
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// ours reports whether pid is the process we started or one of its descendants.
func ours(pid, root int) bool {
	for i := 0; i < 16 && pid > 1; i++ {
		if pid == root {
			return true
		}
		out, err := exec.Command("/bin/ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return false
		}
	}
	return pid == root
}
