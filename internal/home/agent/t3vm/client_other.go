//go:build !unix

package t3vm

import (
	"fmt"
	"io"
	"net"
	"os"
)

// handOver runs the command with this process's input and output and passes its exit status on. A process cannot be replaced by
// another here, so the shim stays alive and keeps the connection to Home open until the command ends, which is what keeps the
// VM of the session.
func handOver(path string, argv []string, conn net.Conn, o ClientOptions, stdout, stderr io.Writer) int {
	defer conn.Close()
	relay := o.Relay
	if relay == nil {
		relay = relayProcess
	}
	code, err := relay(path, argv, o.Environ(), os.Stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "rubi-home agent-vm: can't run %s: %v\n", argv[0], err)
		return 1
	}
	return code
}
