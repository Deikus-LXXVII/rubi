//go:build unix

package t3vm

import (
	"fmt"
	"io"
	"net"
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

// handOver replaces this process with the command, letting it inherit conn (a copy of the descriptor): the connection to Home ends
// when the command does. It returns only when that was not possible, with the exit status to use.
func handOver(path string, argv []string, conn net.Conn, o ClientOptions, _, stderr io.Writer) int {
	f, err := conn.(*net.UnixConn).File() // a copy of the descriptor; the connection stays open while it does
	if err != nil {
		fmt.Fprintf(stderr, "rubi-home agent-vm: can't keep the connection to Rubi Home: %v\n", err)
		return 1
	}
	_ = conn.Close() // the process that replaces this one holds the copy; the connection ends when that process does
	exec := o.Exec
	if exec == nil {
		exec = execProcess
	}
	err = exec(path, argv, o.Environ(), f)
	f.Close()
	fmt.Fprintf(stderr, "rubi-home agent-vm: can't run %s: %v\n", argv[0], err)
	return 1
}
