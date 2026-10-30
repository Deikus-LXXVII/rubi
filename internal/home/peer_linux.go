package home

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// osProcs asks the Linux kernel: SO_PEERCRED for the socket's peer, /proc/<pid>/stat for a process.
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
		var u *unix.Ucred
		if u, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); gerr == nil {
			pid = int(u.Pid)
		}
	}); err != nil {
		return 0, err
	}
	return pid, gerr
}

func (osProcs) Info(pid int) (int, int64, string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, "", err
	}
	// "pid (comm) state ppid ..." where comm may hold spaces and parentheses: cut after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, "", errors.New("unreadable stat")
	}
	name := s[strings.IndexByte(s, '(')+1 : i]
	f := strings.Fields(s[i+1:]) // f[0] is the state (field 3); ppid is field 4, starttime field 22
	if len(f) < 20 {
		return 0, 0, "", errors.New("short stat")
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, "", err
	}
	start, err := strconv.ParseInt(f[19], 10, 64)
	if err != nil {
		return 0, 0, "", err
	}
	return ppid, start, name, nil
}
