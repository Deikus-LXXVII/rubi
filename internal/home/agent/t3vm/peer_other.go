//go:build !darwin && !linux

package t3vm

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("peer credentials aren't available on this system")
}
