package t3vm

import (
	"net"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

func peerPID(c net.Conn) (int, error) { return fsx.PeerPID(c) }

func parentOf(pid int) (int, error) {
	ppid, _, _, err := fsx.ProcInfo(pid)
	return ppid, err
}
