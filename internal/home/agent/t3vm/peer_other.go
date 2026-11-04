//go:build !darwin && !linux && !windows

package t3vm

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials aren't available on this system")
}

func peerIsSelf(net.Conn) bool { return false }
