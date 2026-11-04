//go:build darwin

package t3vm

import (
	"errors"
	"net"
	"os"

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
		var x *unix.Xucred
		x, gerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if gerr == nil {
			uid = x.Uid
		}
	}); err != nil {
		return 0, err
	}
	return uid, gerr
}

// peerIsSelf says whether the process on the other end is this user's, as the kernel says; false when it cannot be told.
func peerIsSelf(c net.Conn) bool {
	uid, err := peerUID(c)
	return err == nil && int(uid) == os.Getuid()
}
