package netpipe

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Frame types.
const (
	tHello byte = 1
	tOpen  byte = 2
	tData  byte = 3
	tClose byte = 4
	tReset byte = 5
	tAck   byte = 6
)

// Services a stream can ask for.
const (
	SvcProxy   byte = 1
	SvcGateway byte = 2
)

// Wire parameters. Both sides must use the same Window.
const (
	Version           = 1
	headerLen         = 9
	MaxPayload        = 32 << 10
	DefaultWindow     = 256 << 10
	DefaultMaxStreams = 64
)

var (
	// ErrProtocol ends the channel: the peer sent something that isn't a valid frame.
	ErrProtocol = errors.New("netpipe: protocol error")
	// ErrTooManyStreams is Open's answer when the limit is reached.
	ErrTooManyStreams = errors.New("netpipe: too many streams")
	// ErrReset is what a stream's reads and writes return after the peer reset it.
	ErrReset = errors.New("netpipe: stream reset")
	// ErrChannelClosed is what streams return once the channel is gone.
	ErrChannelClosed = errors.New("netpipe: channel closed")
)

// Config configures a Mux.
type Config struct {
	// Window is the credit per stream and direction (DefaultWindow when zero).
	Window int
	// MaxStreams bounds the streams open at once (DefaultMaxStreams when zero); more are reset.
	MaxStreams int
	// OnOpen is called, from the reading goroutine (it must not block), for each stream the peer opens. Returning
	// false resets the stream. Nil resets every stream.
	OnOpen func(svc byte, s *Stream) bool
	// Logf receives protocol errors (nil discards).
	Logf func(format string, args ...any)
}

// Mux multiplexes streams over one channel.
type Mux struct {
	cfg Config
	rw  io.ReadWriteCloser

	// Frames are queued and written by one goroutine, so that the reading goroutine (which answers with RESET and ACK)
	// never waits for the peer's reader: two sides that both wait to write can't deadlock. The queue is bounded by
	// the credit of the streams, plus a margin; a peer that lets it overflow isn't reading and the channel fails.
	wq      [][]byte
	wqBytes int
	wake    chan struct{}

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	err     error
	done    chan struct{}
	hello   chan struct{}
	helloed bool
}

// New wraps a channel. Call Run to serve it.
func New(rw io.ReadWriteCloser, cfg Config) *Mux {
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = DefaultMaxStreams
	}
	m := &Mux{cfg: cfg, rw: rw, streams: map[uint32]*Stream{}, done: make(chan struct{}), hello: make(chan struct{}),
		wake: make(chan struct{}, 1)}
	go m.writer()
	return m
}

// Hello is closed when the peer's HELLO arrived (its listeners are up).
func (m *Mux) Hello() <-chan struct{} { return m.hello }

// Done is closed when the channel has ended.
func (m *Mux) Done() <-chan struct{} { return m.done }

// Err is why the channel ended (nil while it is up).
func (m *Mux) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

// Streams is how many streams are open.
func (m *Mux) Streams() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.streams)
}

// Close ends the channel and every stream.
func (m *Mux) Close() error {
	m.fail(ErrChannelClosed)
	return nil
}

func (m *Mux) logf(format string, args ...any) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, args...)
	}
}

// fail ends the channel (once): the first error is kept, the channel is closed and every stream is reset.
func (m *Mux) fail(err error) {
	m.mu.Lock()
	if m.err != nil {
		m.mu.Unlock()
		return
	}
	m.err = err
	all := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		all = append(all, s)
	}
	m.streams = map[uint32]*Stream{}
	close(m.done)
	m.mu.Unlock()
	_ = m.rw.Close()
	for _, s := range all {
		s.abort(err)
	}
}

// send queues one frame; the error is the channel's if it has ended.
func (m *Mux) send(typ byte, id uint32, payload []byte) error {
	buf := make([]byte, headerLen+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:], id)
	binary.BigEndian.PutUint32(buf[5:], uint32(len(payload)))
	copy(buf[headerLen:], payload)
	m.mu.Lock()
	if m.err != nil {
		err := m.err
		m.mu.Unlock()
		return err
	}
	if m.wqBytes+len(buf) > m.cfg.MaxStreams*m.cfg.Window+(1<<20) {
		m.mu.Unlock()
		m.fail(errors.New("netpipe: the peer doesn't read"))
		return m.Err()
	}
	m.wq = append(m.wq, buf)
	m.wqBytes += len(buf)
	m.mu.Unlock()
	signal(m.wake)
	return nil
}

func (m *Mux) writer() {
	for {
		select {
		case <-m.wake:
		case <-m.done:
			return
		}
		for {
			m.mu.Lock()
			if len(m.wq) == 0 {
				m.mu.Unlock()
				break
			}
			b := m.wq[0]
			m.wq = m.wq[1:]
			m.mu.Unlock()
			if _, err := m.rw.Write(b); err != nil {
				m.fail(fmt.Errorf("netpipe: write: %w", err))
				return
			}
			m.mu.Lock()
			m.wqBytes -= len(b)
			m.mu.Unlock()
		}
	}
}

// SendHello tells the peer this side is ready (the VM side does it; tests may).
func (m *Mux) SendHello() error { return m.send(tHello, 0, []byte{Version}) }

func (m *Mux) newStream(id uint32, svc byte) *Stream {
	s := &Stream{m: m, id: id, svc: svc, credit: m.cfg.Window, rsig: make(chan struct{}, 1), wsig: make(chan struct{}, 1),
		rdl: makeDeadline(), wdl: makeDeadline()}
	return s
}

// Open starts a stream to the peer's service.
func (m *Mux) Open(svc byte) (*Stream, error) {
	m.mu.Lock()
	if m.err != nil {
		err := m.err
		m.mu.Unlock()
		return nil, err
	}
	if len(m.streams) >= m.cfg.MaxStreams {
		m.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	m.nextID++
	s := m.newStream(m.nextID, svc)
	m.streams[s.id] = s
	m.mu.Unlock()
	if err := m.send(tOpen, s.id, []byte{svc}); err != nil {
		return nil, err
	}
	return s, nil
}

func (m *Mux) remove(s *Stream) {
	m.mu.Lock()
	if m.streams[s.id] == s {
		delete(m.streams, s.id)
	}
	m.mu.Unlock()
}

func (m *Mux) lookup(id uint32) *Stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

// Run reads frames until the channel ends and returns why (never nil). It resets every stream on the way out.
func (m *Mux) Run() error {
	br := bufio.NewReaderSize(m.rw, 64<<10)
	var hdr [headerLen]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.EOF
			}
			m.fail(fmt.Errorf("netpipe: read: %w", err))
			return m.Err()
		}
		typ, id, n := hdr[0], binary.BigEndian.Uint32(hdr[1:]), binary.BigEndian.Uint32(hdr[5:])
		if n > MaxPayload {
			return m.protocol("frame of %d bytes", n)
		}
		var payload []byte
		if n > 0 {
			payload = make([]byte, n)
			if _, err := io.ReadFull(br, payload); err != nil {
				m.fail(fmt.Errorf("netpipe: read: %w", err))
				return m.Err()
			}
		}
		if err := m.handle(typ, id, payload); err != nil {
			return err
		}
	}
}

func (m *Mux) protocol(format string, args ...any) error {
	err := fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, args...))
	m.logf("netpipe: %v", err)
	m.fail(err)
	return m.Err()
}

func (m *Mux) handle(typ byte, id uint32, payload []byte) error {
	switch typ {
	case tHello:
		if id != 0 || len(payload) != 1 {
			return m.protocol("bad HELLO")
		}
		if payload[0] != Version {
			return m.protocol("peer speaks version %d, this side %d", payload[0], Version)
		}
		m.mu.Lock()
		if !m.helloed {
			m.helloed = true
			close(m.hello)
		}
		m.mu.Unlock()
		return nil
	case tOpen:
		if id == 0 || len(payload) != 1 {
			return m.protocol("bad OPEN")
		}
		m.mu.Lock()
		if m.streams[id] != nil {
			m.mu.Unlock()
			return m.protocol("stream %d opened twice", id)
		}
		if len(m.streams) >= m.cfg.MaxStreams || m.cfg.OnOpen == nil {
			m.mu.Unlock()
			return m.sendErr(tReset, id)
		}
		s := m.newStream(id, payload[0])
		m.streams[id] = s
		m.mu.Unlock()
		if !m.cfg.OnOpen(payload[0], s) {
			m.remove(s)
			s.abort(ErrReset)
			return m.sendErr(tReset, id)
		}
		return nil
	case tData, tClose, tReset, tAck:
		if id == 0 {
			return m.protocol("frame type %d without a stream", typ)
		}
		if typ == tClose && len(payload) != 0 || typ == tReset && len(payload) != 0 || typ == tAck && len(payload) != 4 {
			return m.protocol("bad frame type %d", typ)
		}
		s := m.lookup(id)
		if s == nil {
			return nil // closed meanwhile
		}
		switch typ {
		case tData:
			if !s.onData(payload) {
				m.remove(s)
				s.abort(ErrReset)
				return m.sendErr(tReset, id)
			}
		case tClose:
			s.onClose()
		case tReset:
			m.remove(s)
			s.abort(ErrReset)
		case tAck:
			if !s.onAck(int(binary.BigEndian.Uint32(payload))) {
				m.remove(s)
				s.abort(ErrReset)
				return m.sendErr(tReset, id)
			}
		}
		return nil
	}
	return m.protocol("unknown frame type %d", typ)
}

// sendErr sends a payload-less frame; a write failure ends the channel and is returned.
func (m *Mux) sendErr(typ byte, id uint32) error {
	if err := m.send(typ, id, nil); err != nil {
		return err
	}
	return nil
}

// ---- Stream ----

// Stream is one connection carried by the channel. It is a net.Conn with CloseWrite (a half close).
type Stream struct {
	m   *Mux
	id  uint32
	svc byte

	mu       sync.Mutex
	rbuf     []byte
	consumed int   // bytes handed to the reader and not yet acknowledged
	credit   int   // bytes this side may still send
	rfin     bool  // the peer wrote its last byte
	wfin     bool  // this side did
	closed   bool  // Close was called
	dead     error // reset or channel gone
	rsig     chan struct{}
	wsig     chan struct{}
	rdl, wdl deadline
}

// ID and Service identify the stream.
func (s *Stream) ID() uint32    { return s.id }
func (s *Stream) Service() byte { return s.svc }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *Stream) abort(err error) {
	s.mu.Lock()
	if s.dead == nil {
		s.dead = err
	}
	s.rbuf = nil
	s.mu.Unlock()
	signal(s.rsig)
	signal(s.wsig)
}

// onData stores a chunk; false if the peer broke the window, wrote after its CLOSE, or wrote to a stream this side closed.
func (s *Stream) onData(b []byte) bool {
	s.mu.Lock()
	if s.dead != nil {
		s.mu.Unlock()
		return true // late data for a stream that is already gone
	}
	if s.closed || s.rfin || len(s.rbuf)+len(b) > s.m.cfg.Window {
		s.mu.Unlock()
		return false
	}
	s.rbuf = append(s.rbuf, b...)
	s.mu.Unlock()
	signal(s.rsig)
	return true
}

func (s *Stream) onClose() {
	s.mu.Lock()
	s.rfin = true
	both := s.wfin
	s.mu.Unlock()
	signal(s.rsig)
	if both {
		s.m.remove(s)
	}
}

func (s *Stream) onAck(n int) bool {
	s.mu.Lock()
	if n <= 0 || s.credit+n > s.m.cfg.Window {
		s.mu.Unlock()
		return false
	}
	s.credit += n
	s.mu.Unlock()
	signal(s.wsig)
	return true
}

// Read implements net.Conn.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.mu.Lock()
		if s.dead != nil {
			err := s.dead
			s.mu.Unlock()
			return 0, err
		}
		if s.closed {
			s.mu.Unlock()
			return 0, net.ErrClosed
		}
		if len(s.rbuf) > 0 {
			n := copy(p, s.rbuf)
			s.rbuf = s.rbuf[n:]
			if len(s.rbuf) == 0 {
				s.rbuf = nil
			}
			s.consumed += n
			ack := 0
			if s.consumed >= s.m.cfg.Window/4 {
				ack, s.consumed = s.consumed, 0
			}
			more := len(s.rbuf) > 0
			s.mu.Unlock()
			if more {
				signal(s.rsig)
			}
			if ack > 0 {
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(ack))
				_ = s.m.send(tAck, s.id, b[:])
			}
			return n, nil
		}
		if s.rfin {
			s.mu.Unlock()
			return 0, io.EOF
		}
		s.mu.Unlock()
		select {
		case <-s.rsig:
		case <-s.rdl.wait():
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// Write implements net.Conn: it waits for credit, so a peer that doesn't read stops the writer.
func (s *Stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		s.mu.Lock()
		switch {
		case s.dead != nil:
			err := s.dead
			s.mu.Unlock()
			return total, err
		case s.closed || s.wfin:
			s.mu.Unlock()
			return total, io.ErrClosedPipe
		}
		n := min(len(p), MaxPayload, s.credit)
		if n > 0 {
			s.credit -= n
			s.mu.Unlock()
			if err := s.m.send(tData, s.id, p[:n]); err != nil {
				return total, err
			}
			p, total = p[n:], total+n
			continue
		}
		s.mu.Unlock()
		select {
		case <-s.wsig:
		case <-s.wdl.wait():
			return total, os.ErrDeadlineExceeded
		}
	}
	return total, nil
}

// CloseWrite says this side writes no more; reads go on.
func (s *Stream) CloseWrite() error {
	s.mu.Lock()
	if s.wfin || s.dead != nil || s.closed {
		s.mu.Unlock()
		return nil
	}
	s.wfin = true
	both := s.rfin
	s.mu.Unlock()
	err := s.m.send(tClose, s.id, nil)
	if both {
		s.m.remove(s)
	}
	return err
}

// closeLinger is how long a closed stream waits for the peer's CLOSE before it is reset.
var closeLinger = 30 * time.Second

// Close ends the stream like closing a socket: what was written is delivered, then the peer reads EOF (a CLOSE);
// what the peer sends from now on is refused with a RESET. A peer that never closes its side is reset after
// closeLinger. Unread data is dropped.
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sendClose := !s.wfin && s.dead == nil
	s.wfin = true
	finished := s.rfin || s.dead != nil
	s.rbuf = nil
	s.mu.Unlock()
	signal(s.rsig)
	signal(s.wsig)
	var err error
	if sendClose {
		err = s.m.send(tClose, s.id, nil)
	}
	if finished {
		s.m.remove(s)
		return err
	}
	time.AfterFunc(closeLinger, func() {
		if s.m.lookup(s.id) == s {
			s.m.remove(s)
			s.abort(ErrReset)
			_ = s.m.send(tReset, s.id, nil)
		}
	})
	return err
}

type addr struct{ s string }

func (a addr) Network() string { return "netpipe" }
func (a addr) String() string  { return a.s }

// LocalAddr and RemoteAddr name the stream; there are no network addresses.
func (s *Stream) LocalAddr() net.Addr  { return addr{fmt.Sprintf("netpipe:%d", s.id)} }
func (s *Stream) RemoteAddr() net.Addr { return addr{fmt.Sprintf("vm:%d", s.id)} }

// SetDeadline implements net.Conn.
func (s *Stream) SetDeadline(t time.Time) error {
	s.rdl.set(t)
	s.wdl.set(t)
	return nil
}

// SetReadDeadline implements net.Conn.
func (s *Stream) SetReadDeadline(t time.Time) error { s.rdl.set(t); return nil }

// SetWriteDeadline implements net.Conn.
func (s *Stream) SetWriteDeadline(t time.Time) error { s.wdl.set(t); return nil }

// deadline is net.Pipe's deadline: a channel that is closed when the time has come.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{}
}

func makeDeadline() deadline { return deadline{cancel: make(chan struct{})} }

func closedChan(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer fired: wait for its close
	}
	d.timer = nil
	closed := closedChan(d.cancel)
	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}
	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		cancel := d.cancel
		d.timer = time.AfterFunc(dur, func() { close(cancel) })
		return
	}
	if !closed {
		close(d.cancel)
	}
}

func (d *deadline) wait() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}
