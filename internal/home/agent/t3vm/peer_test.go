package t3vm

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// The peer of a connection made by this very process is this user (on every system that can say), and a connection whose peer
// cannot be named is not: peerIsSelf is the gate of the agent-vm socket and it fails closed.
func TestPeerIsSelfForOwnConnectionAndNotForAnUnnamedPeer(t *testing.T) {
	dir, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	fsx.AllowSocketRoot(filepath.Dir(sock))
	ln, err := fsx.ListenLocal(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan bool, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			got <- false
			return
		}
		defer c.Close()
		got <- peerIsSelf(c)
	}()
	c, err := fsx.DialLocal(sock, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("a connection from this very process was refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	// not a local socket: the peer cannot be named, so it is not accepted
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if peerIsSelf(a) {
		t.Error("a connection with no peer process was accepted")
	}
	if pid, err := peerPID(a); err == nil || pid != 0 {
		t.Errorf("a pipe has the pid %d", pid)
	}
}
