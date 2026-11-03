//go:build linux

package t3vm

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

// peerUID is the user id of the process on the other end of a unix socket, as the kernel says.
func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var gerr error
	if err := rc.Control(func(fd uintptr) {
		var u *unix.Ucred
		u, gerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if gerr == nil {
			uid = u.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, gerr
}
