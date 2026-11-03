package t3vm

import "net"

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 32

// descendsFrom says whether ancestor is one of the parents of pid (not pid itself), by the given parent lookup.
func descendsFrom(pid, ancestor int, parent func(int) (int, error)) bool {
	for i := 0; i < maxAncestors && pid > 1; i++ {
		p, err := parent(pid)
		if err != nil || p <= 0 {
			return false
		}
		if p == ancestor {
			return true
		}
		pid = p
	}
	return false
}

// peerDescendsFrom says whether the process on the other end of the socket is a descendant of ancestor, as the
// kernel says (the socket's peer, then the parents).
func peerDescendsFrom(c net.Conn, ancestor int) bool {
	pid, err := peerPID(c)
	if err != nil || pid <= 1 {
		return false
	}
	return descendsFrom(pid, ancestor, parentOf)
}
