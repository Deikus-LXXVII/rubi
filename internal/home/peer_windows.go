package home

import (
	"net"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// osProcs asks Windows: SIO_AF_UNIX_GETPEERPID for the socket's peer, and the process's own records for a process (parent,
// creation time, program). Anything it cannot read is an error, and an error is a refusal in every caller.
type osProcs struct{}

func (osProcs) PeerPID(c net.Conn) (int, error) { return fsx.PeerPID(c) }

func (osProcs) Info(pid int) (int, int64, string, error) { return fsx.ProcInfo(pid) }
