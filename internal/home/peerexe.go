package home

import "net"

// peerExe is the program file of the process on the other end of a unix socket connection, as the kernel
// knows it; an error where the OS can't say. No cgo: macOS reads it from the kern.procargs2 sysctl.
func peerExe(c net.Conn) (string, error) {
	pid, err := osProcs{}.PeerPID(c)
	if err != nil {
		return "", err
	}
	return procExe(pid)
}
