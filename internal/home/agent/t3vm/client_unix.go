//go:build unix

package t3vm

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// execProcess replaces the process with argv, letting it inherit conn (the descriptor is made inheritable first).
func execProcess(path string, argv, env []string, conn *os.File) error {
	// Go opens descriptors close-on-exec; this one must survive the exec so that the connection to Home lasts
	// as long as the process that replaced us.
	if _, err := unix.FcntlInt(conn.Fd(), unix.F_SETFD, 0); err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}
