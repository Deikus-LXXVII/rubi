package sshtarget

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// Default limits (design section 9.5).
const (
	DefaultPerRole    = 4
	DefaultMaxAge     = 2 * time.Hour
	DefaultIdle       = 10 * time.Minute
	DefaultBuffer     = 8 << 20
	DefaultGrantCheck = 2 * time.Second
	DefaultOpenPerMin = 30
	DefaultAsks       = 2
	chunk             = 32 << 10
)

// Limits bound the streams. Zero values mean the defaults.
type Limits struct {
	PerRole    int           // streams at once per role
	MaxAge     time.Duration // longest life of a stream
	Idle       time.Duration // longest time without data in either direction
	Buffer     int64         // bytes in flight per direction
	GrantCheck time.Duration // how often a live stream re-reads its grant
	OpenPerMin int           // ssh.open requests per role per minute (refused ones count)
	Asks       int           // ssh.ask questions waiting at once per role
}

func (l Limits) withDefaults() Limits {
	if l.PerRole <= 0 {
		l.PerRole = DefaultPerRole
	}
	if l.MaxAge <= 0 {
		l.MaxAge = DefaultMaxAge
	}
	if l.Idle <= 0 {
		l.Idle = DefaultIdle
	}
	if l.Buffer <= 0 {
		l.Buffer = DefaultBuffer
	}
	if l.GrantCheck <= 0 {
		l.GrantCheck = DefaultGrantCheck
	}
	if l.OpenPerMin <= 0 {
		l.OpenPerMin = DefaultOpenPerMin
	}
	if l.Asks <= 0 {
		l.Asks = DefaultAsks
	}
	return l
}

// DialError is a failure to reach or log in to a target. Its text is a fixed sentence that is safe to give to the
// VM (no address, no library error); the detail is for Home's log.
type DialError struct {
	msg   string
	cause error
}

func (e *DialError) Error() string { return e.msg }
func (e *DialError) Unwrap() error { return e.cause }

// The fixed sentences a runner may learn about a failed dial.
const (
	msgNoAnswer     = "the target didn't answer"
	msgLoginFailed  = "login to the target failed"
	msgUnusable     = "the target can't be used right now; ask the user to check it"
	msgHostKeyShown = "host key changed"
)

// ErrHostKeyChanged: the target presented a host key other than the pinned one; nothing was sent to it.
var ErrHostKeyChanged = errors.New("host key changed")

// Config configures a Hub.
type Config struct {
	Store  *Store
	Audit  *Audit
	Limits Limits
	// Granted says whether the role may use the target now; a live stream asks again every GrantCheck and is cut
	// when it says no. Nil: always.
	Granted func(role, target string) bool
	// Redact is applied to a command (on top of the built-in redaction) before it is audited.
	Redact func(string) string
	Logf   func(format string, args ...any)
}

// Notifier sends a notification to the runner that owns a stream.
type Notifier func(method string, params any)

// OpenReq is one stream's request. Role is the key of the VM's role (from Home's own state, not the runner's
// claim); Bot is for the audit only.
type OpenReq struct {
	Role, Project, Bot, Target, Command string
}

// Hub holds the live streams.
type Hub struct {
	cfg Config
	lim Limits

	mu      sync.Mutex
	streams map[string]*Stream
	perRole map[string]int
	closed  bool
	opens   map[string][]time.Time // recent ssh.open requests per role
	asks    map[string]int         // ssh.ask questions waiting per role
	now     func() time.Time
	live    sync.WaitGroup // streams from Open to the end of their run (the audit line included)
}

// NewHub makes a hub.
func NewHub(cfg Config) *Hub {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Hub{cfg: cfg, lim: cfg.Limits.withDefaults(), streams: map[string]*Stream{}, perRole: map[string]int{},
		opens: map[string][]time.Time{}, asks: map[string]int{}, now: time.Now}
}

// Store is the registry the hub dials from.
func (h *Hub) Store() *Store { return h.cfg.Store }

// Audit is the hub's audit.
func (h *Hub) Audit() *Audit { return h.cfg.Audit }

func (h *Hub) clip(cmd string, n int) string { return ClipCommand(cmd, n, h.cfg.Redact) }

// Refused audits a request that was refused before any stream (the reason is a short sentence).
func (h *Hub) Refused(r OpenReq, result, reason string) {
	now := time.Now().UTC().Format(time.RFC3339)
	h.record(Entry{Role: r.Role, Project: r.Project, Bot: r.Bot, Target: r.Target, Command: h.clip(r.Command, CommandClip),
		Start: now, End: now, Result: result, Reason: h.clip(reason, 200)})
}

func (h *Hub) record(e Entry) {
	if h.cfg.Audit == nil {
		return
	}
	if err := h.cfg.Audit.Add(e); err != nil {
		h.cfg.Logf("ssh audit: %v", err)
	}
}

// AllowOpen counts an ssh.open request of the role and says whether it is within the rate limit (a minute's
// worth). Refused requests count too: that is the flood it limits.
func (h *Hub) AllowOpen(role string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	recent := h.opens[role][:0]
	for _, t := range h.opens[role] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= h.lim.OpenPerMin {
		h.opens[role] = recent
		return false
	}
	h.opens[role] = append(recent, now)
	return true
}

// BeginAsk reserves one of the role's slots for a question waiting for the user; ok is false when it has too
// many waiting. Call end when the question is answered.
func (h *Hub) BeginAsk(role string) (end func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.asks[role] >= h.lim.Asks {
		return nil, false
	}
	h.asks[role]++
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			h.asks[role]--
			h.mu.Unlock()
		})
	}, true
}

// Open checks the target and the command, dials with the pinned host key, runs the command and returns the
// stream. Relaying starts when the caller calls Start (after the answer to ssh.open went out). The caller has
// checked the grant and any approval; ctx bounds the stream's life (the runner's channel).
func (h *Hub) Open(ctx context.Context, r OpenReq, notify Notifier) (*Stream, error) {
	t, ok := h.cfg.Store.Get(r.Target)
	if !ok {
		h.Refused(r, "refused", "unknown target")
		return nil, fmt.Errorf("no SSH target %q", r.Target)
	}
	if !t.Allows(r.Command) {
		h.Refused(r, "refused", "command not allowed")
		return nil, errors.New("this command isn't allowed on the target")
	}
	h.mu.Lock()
	switch {
	case h.closed:
		h.mu.Unlock()
		return nil, errors.New("Rubi Home is stopping")
	case h.perRole[r.Role] >= h.lim.PerRole:
		h.mu.Unlock()
		h.Refused(r, "refused", "too many streams")
		return nil, fmt.Errorf("this role has %d SSH streams open already", h.lim.PerRole)
	}
	h.perRole[r.Role]++
	h.mu.Unlock()
	release := func() {
		h.mu.Lock()
		h.perRole[r.Role]--
		h.mu.Unlock()
	}
	start := time.Now()
	client, err := h.dial(ctx, t)
	if err != nil {
		release()
		var de *DialError
		if errors.As(err, &de) {
			h.cfg.Logf("ssh target %s: %v", t.Name, de.cause)
		}
		if errors.Is(err, ErrHostKeyChanged) {
			h.Refused(r, "host key changed", "the target presented another host key; nothing was sent")
		} else {
			h.Refused(r, "error", err.Error())
		}
		return nil, err
	}
	sess, err := client.NewSession()
	var stdin io.WriteCloser
	var stdout, stderr io.Reader
	if err == nil {
		if stdin, err = sess.StdinPipe(); err == nil {
			if stdout, err = sess.StdoutPipe(); err == nil {
				stderr, err = sess.StderrPipe()
			}
		}
	}
	if err == nil && h.cfg.Granted != nil && !h.cfg.Granted(r.Role, r.Target) { // revoked while dialing
		client.Close()
		release()
		h.Refused(r, "refused", "revoked while connecting")
		return nil, errors.New("this SSH target isn't available to this role")
	}
	if err == nil {
		err = sess.Start(r.Command)
	}
	if err != nil {
		client.Close()
		release()
		h.cfg.Logf("ssh target %s: can't start the command: %v", t.Name, err)
		h.Refused(r, "error", "the command couldn't be started on the target")
		return nil, errors.New("the command couldn't be started on the target")
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &Stream{h: h, id: "ss_" + rand.Text(), req: r, ctx: sctx, cancel: cancel, client: client, sess: sess, stdin: stdin,
		stdout: stdout, stderr: stderr, notify: notify, started: start, goCh: make(chan struct{}), fin: make(chan struct{}), inSig: make(chan struct{}, 1)}
	s.cond = sync.NewCond(&s.mu)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		s.cancel()
		client.Close()
		release()
		return nil, errors.New("Rubi Home is stopping")
	}
	h.streams[s.id] = s
	h.live.Add(1)
	h.mu.Unlock()
	s.idle = time.AfterFunc(h.lim.Idle, func() { s.closeWith("idle timeout") })
	s.age = time.AfterFunc(h.lim.MaxAge, func() { s.closeWith("time limit") })
	go s.run()
	return s, nil
}

func hostKeyAlgos(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{k.Type()}
}

// dial connects to the target from this computer and checks the host key against the pinned one.
func (h *Hub) dial(ctx context.Context, t Target) (*ssh.Client, error) {
	pinned, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
	if err != nil || ssh.FingerprintSHA256(pinned) != t.Fingerprint {
		return nil, &DialError{msgUnusable, errors.New("the target's pinned host key is damaged; register it again")}
	}
	signer, err := h.cfg.Store.signer(t.Name)
	if err != nil {
		return nil, &DialError{msgUnusable, err}
	}
	changed := false
	fixed := ssh.FixedHostKey(pinned)
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(host string, remote net.Addr, k ssh.PublicKey) error {
			err := fixed(host, remote, k)
			changed = err != nil
			return err
		},
		HostKeyAlgorithms: hostKeyAlgos(pinned),
		ClientVersion:     "SSH-2.0-RubiHome",
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, err := h.cfg.Store.dial(dctx, "tcp", t.Addr())
	if err != nil {
		return nil, &DialError{msgNoAnswer, fmt.Errorf("can't reach the target: %w", errText(err))}
	}
	_ = conn.SetDeadline(time.Now().Add(2 * dialTimeout))
	stop := context.AfterFunc(dctx, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, t.Addr(), cfg)
	stop()
	if err != nil {
		conn.Close()
		if changed {
			h.cfg.Store.markChanged(t.Name, true)
			return nil, &DialError{msgHostKeyShown, fmt.Errorf("%w: %s presented another host key than the pinned one; refused", ErrHostKeyChanged, t.Name)}
		}
		return nil, &DialError{msgLoginFailed, fmt.Errorf("can't log in to the target: %w", errText(err))}
	}
	_ = conn.SetDeadline(time.Time{})
	h.cfg.Store.markChanged(t.Name, false)
	return ssh.NewClient(c, chans, reqs), nil
}

func (h *Hub) get(owner, id string) *Stream {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.streams[id]; s != nil && s.req.Role == owner {
		return s
	}
	return nil
}

// Data is a ssh.data from the runner of role owner.
func (h *Hub) Data(owner, id string, fd int, b []byte, eof bool) {
	if s := h.get(owner, id); s != nil {
		s.data(fd, b, eof)
	}
}

// Ack is a ssh.ack from the runner: it consumed n bytes of the stream's output.
func (h *Hub) Ack(owner, id string, n int64) {
	if s := h.get(owner, id); s != nil {
		s.ack(n)
	}
}

// Close is a ssh.close from the runner.
func (h *Hub) Close(owner, id string) {
	if s := h.get(owner, id); s != nil {
		s.closeWith("closed by the runner")
	}
}

// CloseWhere cuts every live stream pred says so about and returns how many.
func (h *Hub) CloseWhere(pred func(role, target string) bool, reason string) int {
	h.mu.Lock()
	var hit []*Stream
	for _, s := range h.streams {
		if pred(s.req.Role, s.req.Target) {
			hit = append(hit, s)
		}
	}
	h.mu.Unlock()
	for _, s := range hit {
		s.closeWith(reason)
	}
	return len(hit)
}

// Recheck re-reads the grants of the live streams now and cuts those no longer granted.
func (h *Hub) Recheck() {
	if h.cfg.Granted == nil {
		return
	}
	h.CloseWhere(func(role, target string) bool { return !h.cfg.Granted(role, target) }, "revoked")
}

// Streams is how many streams are live (of one role when role is set).
func (h *Hub) Streams(role string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if role != "" {
		return h.perRole[role]
	}
	return len(h.streams)
}

// CloseAll cuts every stream and refuses new ones; it waits for the streams to be audited.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	h.closed = true
	var all []*Stream
	for _, s := range h.streams {
		all = append(all, s)
	}
	h.mu.Unlock()
	for _, s := range all {
		s.closeWith("Rubi Home stopped")
	}
	wait := make(chan struct{})
	go func() { h.live.Wait(); close(wait) }()
	select {
	case <-wait:
	case <-time.After(5 * time.Second):
	}
}

// Stream is one command running on a target.
type Stream struct {
	h       *Hub
	id      string
	req     OpenReq
	ctx     context.Context
	cancel  context.CancelFunc
	client  *ssh.Client
	sess    *ssh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
	stderr  io.Reader
	notify  Notifier
	started time.Time
	goCh    chan struct{}
	goOnce  sync.Once
	fin     chan struct{}
	idle    *time.Timer
	age     *time.Timer

	reasonMu sync.Mutex
	reason   string

	mu      sync.Mutex
	cond    *sync.Cond
	outUsed int64
	inQ     [][]byte
	inUsed  int64
	inEOF   bool
	inSig   chan struct{}

	bytesIn, bytesOut atomic.Int64
}

// ID is the stream's id.
func (s *Stream) ID() string { return s.id }

func (s *Stream) done() <-chan struct{} { return s.fin }

// Start begins relaying output (call it once the answer to ssh.open is out, so the runner knows the stream).
func (s *Stream) Start() { s.goOnce.Do(func() { close(s.goCh) }) }

func (s *Stream) closeWith(reason string) {
	s.reasonMu.Lock()
	if s.reason == "" {
		s.reason = reason
	}
	s.reasonMu.Unlock()
	s.cancel()
}

func (s *Stream) touch() { s.idle.Reset(s.h.lim.Idle) }

func (s *Stream) run() {
	defer s.h.live.Done()
	defer close(s.fin)
	go func() { // whatever ends the stream hangs up the connection, which ends the readers
		<-s.ctx.Done()
		s.mu.Lock() // wake the pumps waiting for acknowledgements; they check ctx under this lock
		s.cond.Broadcast()
		s.mu.Unlock()
		_ = s.sess.Close()
		_ = s.client.Close()
	}()
	go s.writer()
	go func() {
		t := time.NewTicker(s.h.lim.GrantCheck)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				if g := s.h.cfg.Granted; g != nil && !g(s.req.Role, s.req.Target) {
					s.closeWith("revoked")
					return
				}
			}
		}
	}()
	select {
	case <-s.goCh:
	case <-s.ctx.Done():
	}
	var wg sync.WaitGroup
	for fd, r := range map[int]io.Reader{1: s.stdout, 2: s.stderr} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pump(fd, r)
		}()
	}
	wg.Wait()
	werr := s.sess.Wait()
	s.finish(werr)
}

// pump relays one output stream to the runner, never more than Buffer bytes ahead of its acknowledgements.
func (s *Stream) pump(fd int, r io.Reader) {
	buf := make([]byte, chunk)
	for {
		s.mu.Lock()
		for s.outUsed >= s.h.lim.Buffer && s.ctx.Err() == nil {
			s.cond.Wait()
		}
		room := s.h.lim.Buffer - s.outUsed
		s.mu.Unlock()
		if s.ctx.Err() != nil {
			return
		}
		n, err := r.Read(buf[:min(int64(len(buf)), room)])
		if n > 0 {
			s.mu.Lock()
			s.outUsed += int64(n)
			s.mu.Unlock()
			s.bytesOut.Add(int64(n))
			s.touch()
			s.notify("ssh.data", map[string]any{"stream": s.id, "fd": fd, "b64": base64.StdEncoding.EncodeToString(buf[:n])})
		}
		if err != nil {
			return
		}
	}
}

func (s *Stream) ack(n int64) {
	s.mu.Lock()
	s.outUsed -= min(max(n, 0), s.outUsed)
	s.mu.Unlock()
	s.cond.Broadcast()
}

func (s *Stream) data(fd int, b []byte, eof bool) {
	if fd != 0 {
		return
	}
	s.mu.Lock()
	if s.inEOF {
		s.mu.Unlock()
		return
	}
	if s.inUsed+int64(len(b)) > s.h.lim.Buffer {
		s.mu.Unlock()
		s.closeWith("input buffer overrun")
		return
	}
	if len(b) > 0 {
		s.inQ = append(s.inQ, b)
		s.inUsed += int64(len(b))
	}
	if eof {
		s.inEOF = true
	}
	s.mu.Unlock()
	s.bytesIn.Add(int64(len(b)))
	s.touch()
	select {
	case s.inSig <- struct{}{}:
	default:
	}
}

// writer feeds the runner's input to the command and tells the runner how much it took.
func (s *Stream) writer() {
	dead := false
	for {
		s.mu.Lock()
		if len(s.inQ) == 0 {
			eof := s.inEOF
			s.mu.Unlock()
			if eof {
				_ = s.stdin.Close()
				return
			}
			select {
			case <-s.inSig:
				continue
			case <-s.ctx.Done():
				return
			}
		}
		b := s.inQ[0]
		s.inQ = s.inQ[1:]
		s.mu.Unlock()
		if !dead {
			if _, err := s.stdin.Write(b); err != nil {
				dead = true // the command stopped reading; the rest is dropped but still acknowledged
			}
		}
		s.mu.Lock()
		s.inUsed -= int64(len(b))
		s.mu.Unlock()
		s.notify("ssh.ack", map[string]any{"stream": s.id, "n": len(b)})
	}
}

func (s *Stream) finish(werr error) {
	s.idle.Stop()
	s.age.Stop()
	s.reasonMu.Lock()
	reason := s.reason
	s.reasonMu.Unlock()
	code := 0
	var ee *ssh.ExitError
	switch {
	case werr == nil:
	case errors.As(werr, &ee):
		code = ee.ExitStatus()
		if ee.Signal() != "" {
			code = -1
			if reason == "" {
				reason = "signal " + clipText(ee.Signal(), 20)
			}
		}
	default:
		code = -1
		if reason == "" {
			reason = "the connection ended without an exit status"
		}
	}
	if reason != "" {
		code = -1
	}
	s.cancel()
	s.h.mu.Lock()
	delete(s.h.streams, s.id)
	s.h.perRole[s.req.Role]--
	s.h.mu.Unlock()
	p := map[string]any{"stream": s.id, "code": code}
	if reason != "" {
		p["reason"] = reason
	}
	s.notify("ssh.exit", p)
	result := "ok"
	if reason != "" {
		result = reason
	}
	exit := code
	s.h.record(Entry{Stream: s.id, Role: s.req.Role, Project: s.req.Project, Bot: s.req.Bot, Target: s.req.Target,
		Command: s.h.clip(s.req.Command, CommandClip), Start: s.started.UTC().Format(time.RFC3339),
		End: time.Now().UTC().Format(time.RFC3339), Exit: &exit, BytesIn: s.bytesIn.Load(), BytesOut: s.bytesOut.Load(), Result: result})
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
