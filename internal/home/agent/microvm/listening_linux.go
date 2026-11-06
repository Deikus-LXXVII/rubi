package microvm

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// soAcceptCon is the flag /proc/net/unix shows for a listening socket.
const soAcceptCon = 1 << 16

// listening says whether a Unix socket is listening at path, read from /proc/net/unix without connecting (virtiofsd
// serves one connection: a test connection would be the one). Some virtiofsd versions make the socket's file a while
// before they listen; QEMU's connection in that gap is refused and the VM doesn't start.
func listening(path string) bool {
	f, err := os.Open("/proc/net/unix")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || strings.Join(fields[7:], " ") != path {
			continue
		}
		if flags, err := strconv.ParseUint(fields[3], 16, 32); err == nil && flags&soAcceptCon != 0 {
			return true
		}
	}
	return false
}
