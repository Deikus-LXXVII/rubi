package fsx

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	me    = "S-1-5-21-1111-2222-3333-1001"
	other = "S-1-5-21-1111-2222-3333-1002"
)

func TestUserOnlySDDL(t *testing.T) {
	got, err := UserOnlySDDL(me)
	// protected (nothing inherited from the parent), one entry: full control for this SID, passed on to what is made inside
	if err != nil || got != "D:PAI(A;OICI;FA;;;"+me+")" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "S-1-5", "Everyone", me + ")(A;;FA;;;WD", "S-1-1-0;", "D:(A;;FA;;;WD)", "S-1-5-21-1 ", "s-1-5-21-1-2"} {
		if sddl, err := UserOnlySDDL(bad); err == nil {
			t.Errorf("%q gave %q", bad, sddl)
		}
	}
}

// Only the user (and SYSTEM) may use the folder of the socket: any other trustee with any right, a missing list, or a denied
// user is refused.
func TestCheckUserOnlyACL(t *testing.T) {
	allow := func(sid string, mask uint32) ACE { return ACE{SID: sid, Kind: ACEAllow, Mask: mask} }
	for name, c := range map[string]struct {
		null bool
		aces []ACE
		ok   bool
		why  string
	}{
		"the user alone":                   {false, []ACE{allow(me, 0x1f01ff)}, true, ""},
		"the user and SYSTEM":              {false, []ACE{allow(me, 0x1f01ff), allow(SIDSystem, 0x1f01ff)}, true, ""},
		"what the SDDL makes":              {false, []ACE{{SID: me, Kind: ACEAllow, Mask: 0x1f01ff, Flags: 0x03}}, true, ""},
		"another user":                     {false, []ACE{allow(me, 0x1f01ff), allow(other, 0x1200a9)}, false, other},
		"Everyone":                         {false, []ACE{allow(me, 0x1f01ff), allow("S-1-1-0", 0x1)}, false, "S-1-1-0"},
		"Users":                            {false, []ACE{allow(me, 0x1f01ff), allow(SIDAdministrators, 0x1f01ff), allow("S-1-5-32-545", 0x1200a9)}, false, SIDAdministrators},
		"Authenticated Users":              {false, []ACE{allow(me, 0x1f01ff), allow("S-1-5-11", 0x2)}, false, "S-1-5-11"},
		"INTERACTIVE":                      {false, []ACE{allow(me, 0x1f01ff), allow("S-1-5-4", 0x1)}, false, "S-1-5-4"},
		"no list":                          {true, nil, false, "no access list"},
		"an empty list":                    {false, nil, false, "no access"},
		"a list without the user":          {false, []ACE{allow(SIDSystem, 0x1f01ff)}, false, "no access"},
		"the user denied":                  {false, []ACE{{SID: me, Kind: ACEDeny, Mask: 0x1}, allow(me, 0x1f01ff)}, false, "denies the user"},
		"an entry of an unknown kind":      {false, []ACE{allow(me, 0x1f01ff), {SID: other, Kind: ACEOther}}, false, "not checked"},
		"another user's inherit-only rule": {false, []ACE{allow(me, 0x1f01ff), {SID: other, Kind: ACEAllow, Mask: 0x1f01ff, Flags: 0x08}}, true, ""},
		"a deny for someone else":          {false, []ACE{allow(me, 0x1f01ff), {SID: other, Kind: ACEDeny, Mask: 0x1}}, true, ""},
		"an allow with no rights":          {false, []ACE{allow(me, 0x1f01ff), allow(other, 0)}, true, ""},
	} {
		ok, why := CheckUserOnlyACL(c.null, c.aces, me)
		if ok != c.ok || !strings.Contains(why, c.why) {
			t.Errorf("%s: %v %q", name, ok, why)
		}
	}
	if ok, _ := CheckUserOnlyACL(false, []ACE{allow(me, 1)}, "not a sid"); ok {
		t.Error("an unknown user was accepted")
	}
}

// The decision about a peer: only the same user in the same terminal session and logon, and nothing is allowed when the system
// could not say.
func TestPeerAllowed(t *testing.T) {
	w := func(sid string, session uint32, logon uint64) Who { return Who{sid, session, logon} }
	for name, c := range map[string]struct {
		known      bool
		peer, mine Who
		ok         bool
	}{
		"the same user, session and logon": {true, w(me, 1, 7), w(me, 1, 7), true},
		"the same user, SID in other case": {true, w(strings.ToLower(me), 1, 7), w(me, 1, 7), true},
		"another user":                     {true, w(other, 1, 7), w(me, 1, 7), false},
		"another session of the same user": {true, w(me, 2, 7), w(me, 1, 7), false},
		"the services session":             {true, w(me, 0, 7), w(me, 1, 7), false},
		"another logon, same session":      {true, w(me, 1, 8), w(me, 1, 7), false},
		"SYSTEM":                           {true, w(SIDSystem, 0, 999), w(me, 1, 7), false},
		"unknown peer":                     {false, w(me, 1, 7), w(me, 1, 7), false},
		"no peer SID":                      {true, w("", 1, 7), w(me, 1, 7), false},
		"no SID of ours":                   {true, w(me, 1, 7), w("", 1, 7), false},
	} {
		err := PeerAllowed(c.known, c.peer, c.mine)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A peer process born after the connection was accepted holds a reused process id; one born before it, or in the same instant,
// is the real peer. No creation time is a refusal.
func TestPeerBorn(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for name, c := range map[string]struct {
		created time.Time
		ok      bool
	}{
		"earlier":      {at.Add(-time.Hour), true},
		"same instant": {at, true},
		"later":        {at.Add(time.Nanosecond), false},
		"unknown":      {time.Time{}, false},
	} {
		if err := PeerBorn(c.created, at); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Rubi Home restricts a folder only inside its own folders; a bare root is never one, and a folder next to or above a root,
// or one that only shares a name prefix with it, is not inside.
func TestInsideRoots(t *testing.T) {
	roots := []string{"/data/Rubi Home", "", "/", "/home/u/.rubi-home"}
	for dir, want := range map[string]bool{
		"/data/Rubi Home":            true,
		"/data/Rubi Home/":           true,
		"/data/Rubi Home/sub/deeper": true,
		"/data/Rubi Home/../Other":   false,
		"/data/Rubi Home2":           false,
		"/data":                      false,
		"/home/u/.rubi-home/x":       true,
		"/home/u":                    false,
		"/":                          false,
		"/etc":                       false,
		"/data/rubi home/sub":        false,
		"relative/Rubi Home":         false,
		"/data/Rubi Home/../../etc":  false,
	} {
		if got := InsideRoots(dir, roots, false); got != want {
			t.Errorf("%q: %v, want %v", dir, got, want)
		}
	}
	if !InsideRoots("/data/rubi home/sub", roots, true) {
		t.Error("case-insensitive comparison did not match")
	}
	if InsideRoots("/etc", []string{"/"}, false) || InsideRoots("/etc", nil, false) {
		t.Error("a bare root or no root allowed a folder")
	}
}

// fakeListener hands out connections from a list.
type fakeListener struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (f *fakeListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.conns) == 0 {
		return nil, errors.New("closed")
	}
	c := f.conns[0]
	f.conns = f.conns[1:]
	return c, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return &net.UnixAddr{Name: "fake", Net: "unix"} }

type marked struct {
	net.Conn
	name   string
	closed bool
}

func (m *marked) Close() error { m.closed = true; return nil }

// A connection the check refuses is closed and never handed on; the listener goes on to the next, and an error of the listener
// itself still ends Accept. A peer that cannot be identified is refused, the same as one of another user.
func TestCheckedListenerRefusesAndGoesOn(t *testing.T) {
	a, b, c, d := &marked{name: "other user"}, &marked{name: "unknown"}, &marked{name: "mine"}, &marked{name: "mine too"}
	ln := &fakeListener{conns: []net.Conn{a, b, c, d}}
	var logged []string
	checked := Checked(ln, func(conn net.Conn, _ time.Time) error {
		switch conn.(*marked).name {
		case "other user":
			return PeerAllowed(true, Who{other, 1, 1}, Who{me, 1, 1})
		case "unknown":
			return PeerAllowed(false, Who{}, Who{me, 1, 1})
		}
		return nil
	}, func(f string, args ...any) { logged = append(logged, f) })
	got, err := checked.Accept()
	if err != nil || got != c {
		t.Fatalf("first accepted: %v %v", got, err)
	}
	if !a.closed || !b.closed || c.closed {
		t.Fatalf("closed: other %v unknown %v mine %v", a.closed, b.closed, c.closed)
	}
	if len(logged) != 2 {
		t.Errorf("logged %d refusals", len(logged))
	}
	if got, err := checked.Accept(); err != nil || got != d {
		t.Fatalf("second: %v %v", got, err)
	}
	if _, err := checked.Accept(); err == nil {
		t.Fatal("a closed listener accepted")
	}
}
