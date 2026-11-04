package fsx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The local sockets of Rubi Home (docs/plans/home-platforms.md, stage P6): the hook's socket, the sister tools' socket, the
// agent-vm socket. They are AF_UNIX sockets on every system: the kernel on Unix, Windows 10 and later on Windows (net.Listen
// "unix" works there). ListenLocal and DialLocal are the one way to make and reach them.
//
// Who may connect:
//
//   - Unix: the socket file is mode 0600 in Home's private folder, and the callers ask the kernel who is on the other end
//     (SO_PEERCRED, LOCAL_PEERPID).
//   - Windows: the folder of the socket gets an access list for the current user and nobody else (UserOnlySDDL, checked
//     again by reading it back: CheckUserOnlyACL), and then every connection is checked as it is accepted, in both
//     directions: the peer's process is found with the ioctl SIO_AF_UNIX_GETPEERPID, and its user, its terminal session and its
//     logon must be this process's own (PeerAllowed). Another user, another terminal session of the same user (a remote desktop, a
//     service) or another logon (an elevated token), or a peer that cannot be identified (an older Windows without the ioctl, a process this one may not open) is
//     closed at once. A client does the same to the server it dialled, so a look-alike server of another user is never
//     talked to.
//
// Why AF_UNIX and not a named pipe: a pipe's access list is a stronger object-level gate, but a net.Conn with deadlines and a
// clean Close over overlapped pipe I/O needs a library (go-winio) or several hundred lines of our own; the folder's access
// list plus the per-connection check give the same answer with the standard library's sockets.

// UserOnlySDDL is the security descriptor of a folder only the user may use: a protected access list (nothing inherited) with
// one entry, full control for the user's SID, inherited by what is made inside.
func UserOnlySDDL(sid string) (string, error) {
	if !sidText.MatchString(sid) {
		return "", fmt.Errorf("%q is not a security identifier", sid)
	}
	return "D:PAI(A;OICI;FA;;;" + sid + ")", nil
}

var sidText = regexp.MustCompile(`^S-1-\d+(-\d+){1,14}$`)

// CheckUserOnlyACL says whether a folder with this owner and these entries can be used by the user and nobody else (SYSTEM
// always can): there is an access list, no entry that applies to the folder gives anything to anyone but the user or SYSTEM,
// and the user is not denied.
func CheckUserOnlyACL(nullDACL bool, aces []ACE, userSID string) (ok bool, why string) {
	if !sidText.MatchString(userSID) {
		return false, "the user's identifier is unknown"
	}
	if nullDACL {
		return false, "has no access list, so everyone can use it"
	}
	var bad []string
	mine := false
	for _, a := range aces {
		if a.Flags&aceInheritOnly != 0 {
			continue
		}
		switch a.Kind {
		case ACEDeny:
			if a.SID == userSID {
				return false, "denies the user"
			}
			continue
		case ACEOther:
			return false, "with an access rule of a kind that is not checked (for " + a.SID + ")"
		}
		if a.Mask == 0 {
			continue
		}
		switch a.SID {
		case userSID:
			mine = true
		case SIDSystem:
		default:
			bad = append(bad, a.SID)
		}
	}
	if len(bad) > 0 {
		return false, "usable by " + strings.Join(bad, ", ")
	}
	if !mine {
		return false, "gives the user no access"
	}
	return true, ""
}

// Who is a process as the system names it: its user (SID in text form), its terminal session (the number of the desktop it
// runs on: console, remote desktop, service session 0) and its logon (the token's authentication id, one per logon: an
// elevated token of the same user, or a process started with other credentials, has another). An empty SID is unknown.
type Who struct {
	SID     string
	Session uint32
	Logon   uint64
}

// PeerAllowed is the decision about a connection on Windows, made from what the system said about the process on the other
// end. known is false when the system could not say (no peer process id, the process could not be opened, its user could not
// be read): that is a refusal, not a pass. The peer must be the same user, in the same terminal session and the same logon.
func PeerAllowed(known bool, peer, me Who) error {
	switch {
	case !known:
		return errors.New("the process on the other end could not be identified")
	case me.SID == "" || peer.SID == "":
		return errors.New("the user of the process on the other end is unknown")
	case !strings.EqualFold(peer.SID, me.SID):
		return errors.New("the process on the other end belongs to another user")
	case peer.Session != me.Session:
		return errors.New("the process on the other end is in another Windows session")
	case peer.Logon != me.Logon:
		return errors.New("the process on the other end is in another logon")
	}
	return nil
}

// PeerBorn refuses a peer process that was created after the connection was accepted: the process that connected was already
// running then, so a process born later holds a process id that was reused after the real peer exited.
func PeerBorn(created, accepted time.Time) error {
	if created.IsZero() || created.After(accepted) {
		return errors.New("the process on the other end started after the connection was made")
	}
	return nil
}

// InsideRoots says whether dir is one of roots or inside one. A root that is empty or a bare drive or filesystem root (it has
// no parent) counts for nothing, so that no setting can widen it to a whole disk. fold compares without regard to case
// (Windows).
func InsideRoots(dir string, roots []string, fold bool) bool {
	same := func(a, b string) bool {
		if fold {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	d := filepath.Clean(dir)
	for _, r := range roots {
		if r == "" {
			continue
		}
		r = filepath.Clean(r)
		if filepath.Dir(r) == r {
			continue
		}
		for x := d; ; x = filepath.Dir(x) {
			if same(x, r) {
				return true
			}
			if filepath.Dir(x) == x {
				break
			}
		}
	}
	return false
}

// checkedListener closes the connections whose peer the check refuses, and goes on accepting.
type checkedListener struct {
	net.Listener
	check func(c net.Conn, accepted time.Time) error
	logf  func(format string, args ...any)
}

func (l *checkedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := l.check(c, time.Now()); err != nil {
			if l.logf != nil {
				l.logf("local socket: refused a connection: %v", err)
			}
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// Checked wraps a listener so that a connection is handed on only when check accepts it. check gets the time the connection
// was accepted.
func Checked(ln net.Listener, check func(c net.Conn, accepted time.Time) error, logf func(string, ...any)) net.Listener {
	return &checkedListener{Listener: ln, check: check, logf: logf}
}

// DialLocal connects to a local socket (the timeout bounds the connection).
func DialLocal(path string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return DialLocalContext(ctx, path)
}
