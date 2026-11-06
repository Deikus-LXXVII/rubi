//go:build linux

package guest

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// In a network namespace of its own, as in the VM, loopback is down: listening on 127.0.0.1 fails with EADDRNOTAVAIL
// (what the runner's forwarder hit in the first live agent run) until LoopbackUp, and then works. The check runs in a
// child of the test in a new user and network namespace; where the system allows neither, it is skipped.
func TestLoopbackUpLetsTheForwarderListen(t *testing.T) {
	if os.Getenv("RUBI_TEST_LOOPBACK_CHILD") == "1" {
		loopbackChild()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoopbackUpLetsTheForwarderListen$")
	cmd.Env = append(os.Environ(), "RUBI_TEST_LOOPBACK_CHILD=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Skipf("no network namespace of its own here: %v", err)
	}
	s := string(out)
	switch {
	case strings.Contains(s, "LOOPBACK-OK"):
	case strings.Contains(s, "LOOPBACK-SKIP"):
		t.Skip(s)
	default:
		t.Fatalf("child: %v\n%s", err, s)
	}
}

func loopbackChild() {
	say := func(s string) { os.Stdout.WriteString(s + "\n") }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		ln.Close()
		say("LOOPBACK-SKIP: loopback already up in a new network namespace")
		os.Exit(0)
	}
	if !errors.Is(err, syscall.EADDRNOTAVAIL) {
		say("before: " + err.Error())
		os.Exit(1)
	}
	if err := LoopbackUp(); err != nil {
		say("LoopbackUp: " + err.Error())
		os.Exit(1)
	}
	ln, err = net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		say("after: " + err.Error())
		os.Exit(1)
	}
	ln.Close()
	say("LOOPBACK-OK")
	os.Exit(0)
}
