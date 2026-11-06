package home

import (
	"bytes"
	"encoding/binary"
	"errors"

	"golang.org/x/sys/unix"
)

// procExe is the executable path of a process: kern.procargs2 starts with argc (int32) and then exec_path.
func procExe(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) < 5 {
		return "", errors.New("short procargs")
	}
	rest := raw[binary.Size(int32(0)):]
	if i := bytes.IndexByte(rest, 0); i > 0 {
		return string(rest[:i]), nil
	}
	return "", errors.New("no executable path")
}
