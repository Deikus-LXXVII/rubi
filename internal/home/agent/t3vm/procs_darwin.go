//go:build darwin

package t3vm

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func peerPID(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	pid := 0
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		pid, gerr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	return pid, gerr
}

func parentOf(pid int) (int, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if int(k.Proc.P_pid) != pid {
		return 0, errors.New("no such process")
	}
	return int(k.Eproc.Ppid), nil
}
