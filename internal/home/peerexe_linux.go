package home

import (
	"os"
	"strconv"
)

// procExe is the executable path of a process (/proc/<pid>/exe).
func procExe(pid int) (string, error) { return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe") }
