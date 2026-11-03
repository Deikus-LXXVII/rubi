//go:build !darwin && !linux

package t3vm

import (
	"errors"
	"net"
)

func peerPID(net.Conn) (int, error) {
	return 0, errors.New("peer process ids aren't available on this system")
}
func parentOf(int) (int, error) {
	return 0, errors.New("process trees aren't available on this system")
}
