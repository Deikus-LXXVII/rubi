package hostguard

import "golang.org/x/sys/unix"

// Probe reads this computer's Yama state, kernel and machine id.
func Probe() (Host, error) {
	h, err := ReadHost("/proc", "/etc")
	if err != nil {
		return h, err
	}
	d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return Host{}, err
	}
	h.Dumpable = d != 0
	return h, nil
}

// MemAvailable is this computer's free memory in MiB (ok is false when it can't be read).
func MemAvailable() (int, bool) {
	n, err := ReadMemAvailable("/proc")
	return n, err == nil
}

// SetNonDumpable marks this process non-dumpable: other processes of the same user can no longer read its memory
// through ptrace or /proc/<pid>/mem (root still can). It doesn't carry over an exec, so it protects only the process
// that calls it.
func SetNonDumpable() error { return unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
