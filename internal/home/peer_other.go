//go:build !darwin && !linux

package home

import (
	"errors"
	"net"
)

// osProcs can't identify a peer on this OS, so the sister tools are refused (fail closed).
type osProcs struct{}

func (osProcs) PeerPID(net.Conn) (int, error) {
	return 0, errors.New("peer identification isn't supported on this OS")
}
func (osProcs) Info(int) (int, int64, string, error) {
	return 0, 0, "", errors.New("process lookup isn't supported on this OS")
}
