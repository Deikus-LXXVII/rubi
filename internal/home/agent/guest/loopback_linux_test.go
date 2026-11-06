//go:build linux

package guest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The VM's kernel starts with loopback down, and nothing listens on 127.0.0.1 then (the runner's forwarder got
// EADDRNOTAVAIL in the first live agent run): LoopbackUp brings it up, and the forwarder's port can be listened on. The check runs in a
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
	// the VM's kernel starts with loopback down; a new namespace may have it up already, so take it down first
	if err := loopbackDown(); err != nil {
		say("LOOPBACK-SKIP: can't take loopback down here: " + err.Error())
		os.Exit(0)
	}
	if up, err := loopbackIsUp(); err != nil || up {
		say(fmt.Sprintf("before: up %v, %v", up, err))
		os.Exit(1)
	}
	if err := LoopbackUp(); err != nil {
		say("LoopbackUp: " + err.Error())
		os.Exit(1)
	}
	if up, err := loopbackIsUp(); err != nil || !up {
		say(fmt.Sprintf("after LoopbackUp: up %v, %v", up, err))
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		say("after: " + err.Error())
		os.Exit(1)
	}
	ln.Close()
	say("LOOPBACK-OK")
	os.Exit(0)
}

func loopbackIsUp() (bool, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return false, err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return false, err
	}
	return ifr.Uint16()&unix.IFF_UP != 0, nil
}

// loopbackDown clears IFF_UP on lo, as it is when the VM's kernel starts.
func loopbackDown() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() &^ unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}
