package home

import (
	"bytes"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// osProcs asks the macOS kernel: LOCAL_PEERPID for the socket's peer, the kern.proc.pid sysctl for a process.
type osProcs struct{}

func (osProcs) PeerPID(c net.Conn) (int, error) {
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

func (osProcs) Info(pid int) (int, int64, string, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, 0, "", err
	}
	if int(k.Proc.P_pid) != pid {
		return 0, 0, "", syscall.ESRCH
	}
	st := k.Proc.P_starttime
	comm := k.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return int(k.Eproc.Ppid), int64(st.Sec)*1_000_000 + int64(st.Usec), string(comm), nil
}
