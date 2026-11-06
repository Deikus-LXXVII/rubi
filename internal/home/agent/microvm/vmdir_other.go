//go:build unix && !linux

package microvm

// processOfFolder: the VM backend runs on Linux alone; elsewhere no process is taken for a VM's.
func processOfFolder(pid int, dir string) bool { return false }
