//go:build linux

package t3vm

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

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
		var u *unix.Ucred
		u, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if gerr == nil {
			pid = int(u.Pid)
		}
	}); err != nil {
		return 0, err
	}
	return pid, gerr
}

func parentOf(pid int) (int, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// pid (comm) state ppid ...: comm may hold spaces and parentheses, so go from the last ')'
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, errors.New("unreadable stat")
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, errors.New("unreadable stat")
	}
	return strconv.Atoi(f[1])
}
