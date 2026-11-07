//go:build !linux

package hostguard

// Probe: the check applies only to Linux.
func Probe() (Host, error) { return Host{}, nil }

// MemAvailable isn't read outside Linux.
func MemAvailable() (int, bool) { return 0, false }

// SetNonDumpable does nothing outside Linux.
func SetNonDumpable() error { return nil }
