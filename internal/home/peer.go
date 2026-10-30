package home

// Who is calling: the kernel's answer, anchored on the session's own `claude` process. A Claude Code session
// is one `claude` process; its hooks, its MCP servers and whatever its Bash tool starts are descendants of
// it. Right after Accept the helper asks the kernel for the peer's pid (and reads its start time). For a hook
// line of session S it walks up (at most peerMaxDepth levels) to the first ancestor named "claude" and
// records that one process, with its start time, as S's; without such an ancestor it records the peer's
// direct parent. Shells, terminals and the desktop app above it are never recorded. For a RubiTool call it
// finds the caller's anchor the same way and looks up exactly that process; no entry, no tools.
//
// The first session to claim a process keeps it: a hook line naming another session from a process that
// belongs to a live session is ignored (and logged once). A claim ends with its session (SessionEnd), when
// the process's start time no longer matches, or when the session's hooks have been silent for 24 hours.
// What the line says about its session or directory is never used to attribute a tool call. The hook lines
// themselves are trusted as before: a process of the same user can always send a hook line, and the
// boundary is the session's process tree. Where the kernel can't name the peer, tools are refused.

import (
	"errors"
	"log"
	"net"
	"sync"
	"time"
)

const (
	peerMaxDepth   = 6    // ancestors looked at
	peerMaxEntries = 2000 // processes remembered
	peerClaimTTL   = 24 * time.Hour
	// peerTakeoverIdle: a claim whose session has sent no hook for this long may be taken over by a hook line
	// for another session from the same claude process (a SessionEnd that never arrived).
	peerTakeoverIdle = 10 * time.Minute
	claudeProcName   = "claude"
)

// procTree answers the kernel questions (a seam so tests can fake a process tree).
type procTree interface {
	// PeerPID is the process id on the other end of a unix socket connection.
	PeerPID(c net.Conn) (int, error)
	// Info is a process's parent, its start time (an opaque value that changes when the pid is reused) and
	// its name (the executable's, as `ps -o comm` shows it, possibly cut short).
	Info(pid int) (ppid int, start int64, name string, err error)
}

var errNoPeer = errors.New("this session isn't known to Rubi Home yet (or the caller can't be identified); try again after its next prompt")

// peerID is the peer of a connection as the kernel said right after Accept.
type peerID struct {
	pid   int
	start int64
	ok    bool
}

// peerOf reads the peer's pid and start time (called before the connection's line is read).
func (c *Claude) peerOf(conn net.Conn) peerID {
	t := c.procs()
	pid, err := t.PeerPID(conn)
	if err != nil || pid <= 1 {
		return peerID{}
	}
	_, start, _, err := t.Info(pid)
	if err != nil {
		return peerID{}
	}
	return peerID{pid, start, true}
}

type ancestor struct {
	pid   int
	start int64
}

// anchor is the process that stands for the peer's session: the first ancestor named "claude" within
// peerMaxDepth levels, else the peer's direct parent. It fails if the peer is gone or its pid was reused
// since Accept.
func anchor(t procTree, p peerID) (ancestor, bool) {
	if !p.ok {
		return ancestor{}, false
	}
	ppid, start, _, err := t.Info(p.pid)
	if err != nil || start != p.start || ppid <= 1 {
		return ancestor{}, false
	}
	var first ancestor
	cur := ppid
	for depth := 0; depth < peerMaxDepth && cur > 1; depth++ {
		next, st, name, err := t.Info(cur)
		if err != nil {
			break
		}
		a := ancestor{cur, st}
		if depth == 0 {
			first = a
		}
		if name == claudeProcName {
			return a, true
		}
		cur = next
	}
	if first.pid == 0 {
		return ancestor{}, false
	}
	return first, true
}

// peerEntry is a process claimed by one session.
type peerEntry struct {
	start   int64
	session string
	at      time.Time // the session's last hook from this process
}

// peerMap holds process -> session. Its lock is taken alone; nothing slow runs under it.
type peerMap struct {
	mu sync.Mutex
	m  map[int]*peerEntry
}

// note records that process a hosts session s unless another session holds it (a claim ends with forget, a
// start time that no longer matches, or peerClaimTTL without a hook). It reports whether s holds the process
// afterwards. It touches nothing but the map, so a hook line never waits on the session registry.
func (p *peerMap) note(s string, a ancestor, now time.Time) (held, tookOver bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[int]*peerEntry{}
	}
	if e := p.m[a.pid]; e != nil && e.start == a.start && e.session != s && now.Sub(e.at) <= peerTakeoverIdle {
		return false, false // the first claim wins
	} else if e != nil && e.start == a.start && e.session != s && now.Sub(e.at) <= peerClaimTTL {
		tookOver = true // its session has been silent for a while: a missed SessionEnd
	}
	if e := p.m[a.pid]; e == nil && len(p.m) >= peerMaxEntries { // forget the one seen longest ago
		old, oldAt := 0, now.Add(time.Hour)
		for k, v := range p.m {
			if v.at.Before(oldAt) {
				old, oldAt = k, v.at
			}
		}
		delete(p.m, old)
	}
	p.m[a.pid] = &peerEntry{start: a.start, session: s, at: now}
	return true, tookOver
}

// release drops session s's claim on the process a, only if it holds it: a SessionEnd line from another
// process tree releases nothing.
func (p *peerMap) release(s string, a ancestor) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.m[a.pid]; e != nil && e.session == s && e.start == a.start {
		delete(p.m, a.pid)
	}
}

// forget drops every claim of a session (used by tests and when the helper itself knows it ended).
func (p *peerMap) forget(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, e := range p.m {
		if e.session == s {
			delete(p.m, k)
		}
	}
}

// lookup is the session that holds the process (with the start time it was recorded with), if its claim
// hasn't gone quiet for peerClaimTTL.
func (p *peerMap) lookup(a ancestor, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[a.pid]
	if e == nil || e.start != a.start || now.Sub(e.at) > peerClaimTTL {
		return "", false
	}
	return e.session, true
}

func (c *Claude) procs() procTree {
	if c.Procs != nil {
		return c.Procs
	}
	return osProcs{}
}

// attestHook records the hook peer's claude process as session s's (unless a live session holds it).
func (c *Claude) attestHook(p peerID, s string) {
	a, ok := anchor(c.procs(), p)
	if !ok {
		return
	}
	held, tookOver := c.peers.note(s, a, time.Now())
	if tookOver {
		log.Printf("claude: a claude process whose session was silent for %s now sends hooks for another session; the claim moved", peerTakeoverIdle)
	}
	if !held && c.peerOnce.CompareAndSwap(false, true) {
		log.Printf("claude: a hook line for one session came from a process of another live session and was ignored")
	}
}

// releaseHook honors a SessionEnd line: the session's claim is dropped only when the line comes from the
// process that holds it (a SessionEnd from another tree is ignored for the claim map).
func (c *Claude) releaseHook(p peerID, s string) {
	if a, ok := anchor(c.procs(), p); ok {
		c.peers.release(s, a)
	}
}

// sessionOfPeer is the session a tool call's peer belongs to.
func (c *Claude) sessionOfPeer(p peerID) (string, error) {
	a, ok := anchor(c.procs(), p)
	if !ok {
		return "", errNoPeer
	}
	s, ok := c.peers.lookup(a, time.Now())
	if !ok {
		return "", errNoPeer
	}
	return s, nil
}
