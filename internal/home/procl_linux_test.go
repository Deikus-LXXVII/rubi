package home

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"
)

// procListeners finds this process's own listening socket and loses it when it is closed, and doesn't count another
// process's (the live netless test's check that Home opens no listener for a VM, on a Linux without lsof).
func TestProcListenersSeesOwnListener(t *testing.T) {
	if os.Getenv("RUBI_TEST_LISTEN_CHILD") == "1" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(1)
		}
		defer ln.Close()
		os.Stdout.WriteString(fmt.Sprintf("listening %04X\n", ln.Addr().(*net.TCPAddr).Port))
		_ = os.Stdout.Sync()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestProcListenersSeesOwnListener$")
	child.Env = append(os.Environ(), "RUBI_TEST_LISTEN_CHILD=1")
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	line, _ := bufio.NewReader(out).ReadString('\n')
	port, ok := strings.CutPrefix(strings.TrimSpace(line), "listening ")
	if !ok {
		t.Fatalf("child: %q", line)
	}
	for _, l := range procListeners() {
		if strings.HasSuffix(l, ":"+port) {
			t.Fatalf("another process's listener %s is counted", l)
		}
	}
	before := len(procListeners()) // other tests of this process may have listeners of their own
	if again := len(procListeners()); again != before {
		t.Fatalf("listeners %d, then %d", before, again)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(procListeners()); n != before+1 {
		ln.Close()
		t.Fatalf("listeners %d with one of its own more than %d", n, before)
	}
	ln.Close()
	if n := len(procListeners()); n != before {
		t.Fatalf("listeners %d after closing", n)
	}
}

func procListeners() []string {
	mine := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	for _, fd := range fds {
		if l, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.HasPrefix(l, "socket:[") {
			mine[strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]")] = true
		}
	}
	l := []string{}
	for _, f := range []string{"/proc/self/net/tcp", "/proc/self/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			c := strings.Fields(line)
			if len(c) > 9 && c[3] == "0A" && mine[c[9]] {
				l = append(l, c[1])
			}
		}
	}
	sort.Strings(l)
	return l
}
