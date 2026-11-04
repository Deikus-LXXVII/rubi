package t3vm

import (
	"errors"
	"net"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// peerUID has no meaning on Windows (there are no user ids); the check is peerIsSelf.
func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("Windows has no user ids; the peer is checked by its user and logon session")
}

// peerIsSelf says whether the process on the other end is this user's, in this logon session; false when it cannot be told.
func peerIsSelf(c net.Conn) bool { return fsx.PeerSelf(c) == nil }
