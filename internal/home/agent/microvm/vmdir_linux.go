package microvm

import (
	"os"
	"strconv"
)

// processOfFolder: the process's command line, from /proc, names a file of dir.
func processOfFolder(pid int, dir string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	return err == nil && cmdlineNames(b, dir)
}
