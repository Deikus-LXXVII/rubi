//go:build unix

package t3

import (
	"os/exec"
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

// ours reports whether pid is the process we started or one of its descendants. The parent comes from /proc on
// Linux (a box may have no ps) and from the kernel's process table on macOS.
func ours(pid, root int) bool {
	for i := 0; i < 16 && pid > 1; i++ {
		if pid == root {
			return true
		}
		st, err := procOf(pid)
		if err != nil {
			return false
		}
		pid = st.PPID
	}
	return pid == root
}

// stopAdopted ends the group of a server an earlier run left (it isn't our child, so there is nothing to wait
// for: done closes when the leader is gone). still re-checks, right before the signal, that the leader is still
// that server, so a process that got its id in the meantime is never signalled.
func stopAdopted(pgid int, done <-chan struct{}, still func() bool) {
	select {
	case <-done:
		return
	default:
	}
	if !still() {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	// As in stop: the group id stays reserved while any member is left, so this reaches only the server's children.
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// sameUser reports whether this process may signal pid (as a non-root user: pid is the same user's).
func sameUser(pid int) bool { return syscall.Geteuid() != 0 && syscall.Kill(pid, 0) == nil }
