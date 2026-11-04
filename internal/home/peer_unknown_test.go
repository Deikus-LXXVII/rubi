package home

import (
	"net"
	"testing"
)

// A peer the system cannot name is no peer: a socket that gives no process id, a process id of 0 or 1, and a process that
// cannot be read (no start time, no parent) all end in an empty peerID, and an empty peerID has no anchor, so no session and no
// tools. This holds for every system's osProcs, Windows's included (it answers an error for what it cannot open).
func TestAnUnknownPeerIsRefused(t *testing.T) {
	for name, setup := range map[string]func(f *fakeProcs){
		"no peer pid":                      func(f *fakeProcs) { f.noPeer = true },
		"pid 0 (even if it were readable)": func(f *fakeProcs) { f.add(0, 1); f.peer = 0 },
		"pid 1 (even if it were readable)": func(f *fakeProcs) { f.add(1, 0); f.peer = 1 },
		"a process nobody can read":        func(f *fakeProcs) { f.peer = 4242 }, // not in the table: Info answers an error
	} {
		f := newFakeProcs()
		setup(f)
		c := &Claude{Procs: f}
		p := c.peerOf(nil)
		if p.ok {
			t.Errorf("%s: peerOf accepted %+v", name, p)
		}
		if a, ok := anchor(f, p); ok {
			t.Errorf("%s: an anchor %+v for an unknown peer", name, a)
		}
	}
	// a process that is known is accepted (the control)
	f := newFakeProcs()
	f.add(500, 1)
	f.peer = 500
	if p := (&Claude{Procs: f}).peerOf(net.Conn(nil)); !p.ok || p.pid != 500 {
		t.Errorf("a known process was refused: %+v", p)
	}
	// the real thing: a nil or foreign connection has no pid on any system
	if _, err := (osProcs{}).PeerPID(nil); err == nil {
		t.Error("a nil connection has a peer")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if pid, err := (osProcs{}).PeerPID(a); err == nil {
		t.Errorf("an in-memory connection has the peer %d", pid)
	}
	if _, _, _, err := (osProcs{}).Info(1 << 30); err == nil {
		t.Error("a process that does not exist has an info")
	}
}
