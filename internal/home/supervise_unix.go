//go:build unix

package home

import (
	"os"
	"os/exec"
	"syscall"
)

// detach makes the supervisor its own session, so it outlives the shell or tool that ran start.
func detach(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
