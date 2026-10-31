package netpipe

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// pair joins two muxes with an in-memory duplex. a opens, b serves with onOpen.
func pair(t *testing.T, ca, cb Config) (a, b *Mux) {
	t.Helper()
	x, y := net.Pipe()
	a, b = New(x, ca), New(y, cb)
	go a.Run()
	go b.Run()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func echo(svc byte, s *Stream) bool {
	go func() {
		io.Copy(s, s)
		s.CloseWrite()
		s.Close()
	}()
	return true
}

func rnd(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func within(t *testing.T, what string, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("timed out: %s", what)
	}
}

func TestRoundTripBothDirections(t *testing.T) {
	a, _ := pair(t, Config{}, Config{OnOpen: echo})
	s, err := a.Open(SvcProxy)
	if err != nil {
		t.Fatal(err)
	}
	data := rnd(3<<20 + 17) // several windows, an odd size
	var got []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		got, _ = io.ReadAll(s)
	}()
	within(t, "write", 20*time.Second, func() {
		if n, err := s.Write(data); err != nil || n != len(data) {
			t.Errorf("write: %d %v", n, err)
		}
		s.CloseWrite()
	})
	wg.Wait()
	if !bytes.Equal(got, data) {
		t.Fatalf("echoed %d bytes, differing from the %d sent", len(got), len(data))
	}
}

func TestBinarySafe(t *testing.T) {
	a, _ := pair(t, Config{}, Config{OnOpen: echo})
	s, _ := a.Open(SvcGateway)
	all := make([]byte, 0, 256*4)
	for i := 0; i < 1024; i++ {
		all = append(all, byte(i))
	}
	all = append(all, []byte("\r\n\r\n\x00\xff")...)
	go func() { s.Write(all); s.CloseWrite() }()
	var got []byte
	within(t, "read", 5*time.Second, func() { got, _ = io.ReadAll(s) })
	if !bytes.Equal(got, all) {
		t.Fatal("bytes changed on the way")
	}
}

func TestServiceIsPassed(t *testing.T) {
	seen := make(chan byte, 2)
	a, _ := pair(t, Config{}, Config{OnOpen: func(svc byte, s *Stream) bool { seen <- svc; return echo(svc, s) }})
	for _, svc := range []byte{SvcProxy, SvcGateway} {
		if _, err := a.Open(svc); err != nil {
			t.Fatal(err)
		}
		if got := <-seen; got != svc {
			t.Fatalf("service %d arrived as %d", svc, got)
		}
	}
}

func TestFlowControlStopsTheWriter(t *testing.T) {
	const win = 8 << 10
	got := make(chan *Stream, 1)
	a, _ := pair(t, Config{Window: win}, Config{Window: win, OnOpen: func(_ byte, s *Stream) bool { got <- s; return true }})
	s, _ := a.Open(SvcProxy)
	r := <-got // the receiver doesn't read
	s.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := s.Write(make([]byte, 4*win))
	if !errors.Is(err, os.ErrDeadlineExceeded) || n != win {
		t.Fatalf("a writer facing a reader that doesn't read: wrote %d (want %d), err %v", n, win, err)
	}
	// The receiver never holds more than the window.
	waitFor(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.rbuf) == win })
	// Reading gives credit back and the writer goes on.
	s.SetWriteDeadline(time.Time{})
	done := make(chan int)
	go func() { n, _ := s.Write(make([]byte, 3*win)); done <- n }()
	go io.Copy(io.Discard, r)
	select {
	case n := <-done:
		if n != 3*win {
			t.Fatalf("wrote %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer stayed blocked after the reader drained")
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

func TestSlowStreamDoesNotBlockAnother(t *testing.T) {
	var mu sync.Mutex
	var stuck *Stream
	a, _ := pair(t, Config{Window: 4 << 10}, Config{Window: 4 << 10, OnOpen: func(svc byte, s *Stream) bool {
		mu.Lock()
		defer mu.Unlock()
		if stuck == nil {
			stuck = s // never read
			return true
		}
		return echo(svc, s)
	}})
	slow, _ := a.Open(SvcProxy)
	slow.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	slow.Write(make([]byte, 64<<10)) // fills its window and stops
	fast, _ := a.Open(SvcProxy)
	go func() { fast.Write(rnd(100 << 10)); fast.CloseWrite() }()
	var n int64
	within(t, "second stream", 5*time.Second, func() { n, _ = io.Copy(io.Discard, fast) })
	if n != 100<<10 {
		t.Fatalf("second stream moved %d bytes", n)
	}
}

func TestManyConcurrentStreams(t *testing.T) {
	a, b := pair(t, Config{}, Config{OnOpen: echo})
	const n = 60
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := a.Open(SvcProxy)
			if err != nil {
				t.Error(err)
				return
			}
			data := rnd(300<<10 + i)
			go func() { s.Write(data); s.CloseWrite() }()
			got, err := io.ReadAll(s)
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("stream %d: %d bytes back of %d, err %v", i, len(got), len(data), err)
			}
			s.Close()
		}()
	}
	within(t, "all streams", 60*time.Second, wg.Wait)
	waitFor(t, func() bool { return a.Streams() == 0 && b.Streams() == 0 })
}

func TestMaxStreams(t *testing.T) {
	// The opening side refuses at its own limit.
	a, _ := pair(t, Config{MaxStreams: 2}, Config{OnOpen: echo})
	s1, _ := a.Open(SvcProxy)
	a.Open(SvcProxy)
	if _, err := a.Open(SvcProxy); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("third open: %v", err)
	}
	// Closing one frees a place.
	s1.Close()
	waitFor(t, func() bool { _, err := a.Open(SvcProxy); return err == nil })

	// The serving side resets what is over its limit, and the channel lives on.
	c, d := pair(t, Config{}, Config{MaxStreams: 2, OnOpen: echo})
	var ss []*Stream
	for i := 0; i < 3; i++ {
		s, err := c.Open(SvcProxy)
		if err != nil {
			t.Fatal(err)
		}
		ss = append(ss, s)
	}
	buf := make([]byte, 1)
	ss[2].SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := ss[2].Read(buf); !errors.Is(err, ErrReset) {
		t.Fatalf("stream over the limit: %v", err)
	}
	if d.Err() != nil || c.Err() != nil {
		t.Fatalf("the channel ended: %v %v", d.Err(), c.Err())
	}
	ss[0].Write([]byte("x"))
	ss[0].SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := ss[0].Read(buf); err != nil {
		t.Fatalf("a stream within the limit: %v", err)
	}
}

func TestHalfClose(t *testing.T) {
	srv := make(chan *Stream, 1)
	a, b := pair(t, Config{}, Config{OnOpen: func(_ byte, s *Stream) bool { srv <- s; return true }})
	c, _ := a.Open(SvcProxy)
	s := <-srv
	c.Write([]byte("request"))
	c.CloseWrite()
	got, err := io.ReadAll(s) // the server sees the request, then EOF
	if err != nil || string(got) != "request" {
		t.Fatalf("server read %q %v", got, err)
	}
	// The client's write side is shut, its read side is not.
	if _, err := c.Write([]byte("more")); err == nil {
		t.Fatal("write after CloseWrite succeeded")
	}
	s.Write([]byte("response"))
	s.CloseWrite()
	got, err = io.ReadAll(c)
	if err != nil || string(got) != "response" {
		t.Fatalf("client read %q %v", got, err)
	}
	waitFor(t, func() bool { return a.Streams() == 0 && b.Streams() == 0 })
	// A clean end needs no reset: closing now leaves the channel quiet.
	c.Close()
	s.Close()
}

func TestCloseResetsThePeer(t *testing.T) {
	srv := make(chan *Stream, 1)
	a, b := pair(t, Config{}, Config{OnOpen: func(_ byte, s *Stream) bool { srv <- s; return true }})
	c, _ := a.Open(SvcProxy)
	s := <-srv
	c.Write([]byte("partial"))
	c.Close()
	buf := make([]byte, 100)
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := s.Read(buf)
	if err != nil && !errors.Is(err, ErrReset) {
		t.Fatalf("read after the peer's Close: %v", err)
	}
	_, err = s.Read(buf)
	if !errors.Is(err, ErrReset) {
		t.Fatalf("expected a reset, got %v", err)
	}
	if _, err := s.Write([]byte("x")); !errors.Is(err, ErrReset) {
		t.Fatalf("write on a reset stream: %v", err)
	}
	waitFor(t, func() bool { return a.Streams() == 0 && b.Streams() == 0 })
}

func TestOnOpenRefuses(t *testing.T) {
	a, _ := pair(t, Config{}, Config{OnOpen: func(svc byte, s *Stream) bool { return false }})
	s, _ := a.Open(SvcGateway)
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatalf("refused stream: %v", err)
	}
	// No OnOpen at all: the same.
	c, _ := pair(t, Config{}, Config{})
	s, _ = c.Open(SvcGateway)
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatalf("stream to a mux with no handler: %v", err)
	}
}

func TestDeadlines(t *testing.T) {
	srv := make(chan *Stream, 1)
	a, _ := pair(t, Config{}, Config{OnOpen: func(_ byte, s *Stream) bool { srv <- s; return true }})
	c, _ := a.Open(SvcProxy)
	<-srv
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	// A deadline can be moved: the stream is usable afterwards.
	c.SetReadDeadline(time.Time{})
	var ne net.Error
	c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("not a timeout error: %v", err)
	}
}

func TestChannelDeath(t *testing.T) {
	x, y := net.Pipe()
	a := New(x, Config{})
	b := New(y, Config{OnOpen: echo})
	runA, runB := make(chan error, 1), make(chan error, 1)
	go func() { runA <- a.Run() }()
	go func() { runB <- b.Run() }()
	s, _ := a.Open(SvcProxy)
	s2, _ := a.Open(SvcProxy)
	reading := make(chan error, 1)
	go func() { _, err := s2.Read(make([]byte, 1)); reading <- err }()
	time.Sleep(50 * time.Millisecond)
	y.Close() // the exec process died
	select {
	case err := <-runA:
		if err == nil {
			t.Fatal("Run returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run didn't return when the channel died")
	}
	<-runB
	select {
	case err := <-reading:
		if err == nil {
			t.Fatal("a blocked read returned no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a blocked read didn't return")
	}
	if _, err := s.Write([]byte("x")); err == nil {
		t.Fatal("write on a dead channel succeeded")
	}
	if _, err := a.Open(SvcProxy); err == nil {
		t.Fatal("open on a dead channel succeeded")
	}
	select {
	case <-a.Done():
	default:
		t.Fatal("Done isn't closed")
	}
	if a.Streams() != 0 {
		t.Fatal("streams are left")
	}
}

// ---- malformed input, from a raw peer ----

func hdr(typ byte, id uint32, n uint32) []byte {
	b := make([]byte, headerLen)
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:], id)
	binary.BigEndian.PutUint32(b[5:], n)
	return b
}

func fr(typ byte, id uint32, payload ...byte) []byte {
	return append(hdr(typ, id, uint32(len(payload))), payload...)
}

// raw is a mux under test with a hand-driven peer; frames it sends are collected.
type raw struct {
	m      *Mux
	peer   net.Conn
	run    chan error
	opened chan *Stream
	mu     sync.Mutex
	frames []string
}

func newRaw(t *testing.T, cfg Config) *raw {
	x, y := net.Pipe()
	r := &raw{peer: y, run: make(chan error, 1), opened: make(chan *Stream, 8)}
	cfg.OnOpen = func(_ byte, s *Stream) bool { r.opened <- s; return true }
	r.m = New(x, cfg)
	go func() { r.run <- r.m.Run() }()
	go func() { // decode what the mux sends
		for {
			h := make([]byte, headerLen)
			if _, err := io.ReadFull(y, h); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(h[5:])
			p := make([]byte, n)
			io.ReadFull(y, p)
			r.mu.Lock()
			r.frames = append(r.frames, string([]byte{h[0]})+string(h[1:5])+string(p))
			r.mu.Unlock()
		}
	}()
	t.Cleanup(func() { r.m.Close(); y.Close() })
	return r
}

func (r *raw) sent(typ byte, id uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := string([]byte{typ}) + string(hdr(0, id, 0)[1:5])
	for _, f := range r.frames {
		if len(f) >= 5 && f[:5] == want {
			return true
		}
	}
	return false
}

func (r *raw) write(b ...[]byte) {
	for _, x := range b {
		r.peer.Write(x)
	}
}

func (r *raw) wantProtocolError(t *testing.T) {
	t.Helper()
	select {
	case err := <-r.run:
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("Run ended with %v, not a protocol error", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the channel survived a malformed frame")
	}
}

func TestMalformedFramesEndTheChannel(t *testing.T) {
	cases := map[string][]byte{
		"unknown type":         fr(99, 1),
		"zero type":            fr(0, 1),
		"oversize payload":     hdr(tData, 1, MaxPayload+1),
		"huge payload":         hdr(tData, 1, 0xffffffff),
		"data without stream":  fr(tData, 0, 1),
		"close without stream": fr(tClose, 0),
		"open without stream":  fr(tOpen, 0, SvcProxy),
		"open without service": fr(tOpen, 1),
		"open with two bytes":  fr(tOpen, 1, 1, 1),
		"ack of 3 bytes":       fr(tAck, 1, 0, 0, 1),
		"close with payload":   fr(tClose, 1, 1),
		"reset with payload":   fr(tReset, 1, 1),
		"hello with a stream":  fr(tHello, 5, Version),
		"hello empty":          fr(tHello, 0),
		"hello wrong version":  fr(tHello, 0, Version+1),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRaw(t, Config{})
			r.write(b)
			r.wantProtocolError(t)
			select {
			case <-r.m.Done():
			default:
				t.Fatal("Done isn't closed")
			}
		})
	}
}

func TestTruncatedFrameEndsTheChannel(t *testing.T) {
	r := newRaw(t, Config{})
	r.write(hdr(tData, 1, 100)[:5])
	r.peer.Close()
	select {
	case err := <-r.run:
		if err == nil {
			t.Fatal("nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no end")
	}
}

func TestDuplicateOpenEndsTheChannel(t *testing.T) {
	r := newRaw(t, Config{})
	r.write(fr(tOpen, 7, SvcProxy))
	<-r.opened
	r.write(fr(tOpen, 7, SvcProxy))
	r.wantProtocolError(t)
}

func TestStreamLevelViolationsResetOnlyTheStream(t *testing.T) {
	r := newRaw(t, Config{Window: 1024})
	r.write(fr(tOpen, 1, SvcProxy), fr(tOpen, 2, SvcProxy))
	s1, s2 := <-r.opened, <-r.opened
	// More data than the window.
	r.write(append(hdr(tData, 1, 1025-1), make([]byte, 1024)...))
	r.write(append(hdr(tData, 1, 2), 1, 2))
	waitFor(t, func() bool { return r.sent(tReset, 1) })
	if _, err := s1.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatalf("overrun stream: %v", err)
	}
	// An ACK for bytes that were never sent.
	r.write(fr(tAck, 2, 0, 0, 0, 5))
	waitFor(t, func() bool { return r.sent(tReset, 2) })
	if _, err := s2.Write([]byte("x")); !errors.Is(err, ErrReset) {
		t.Fatalf("stream with a bogus ack: %v", err)
	}
	// Frames for streams that don't exist are ignored, and the channel still works.
	r.write(fr(tData, 99, 1, 2, 3), fr(tClose, 99), fr(tReset, 99), fr(tAck, 99, 0, 0, 0, 1))
	r.write(fr(tOpen, 3, SvcGateway))
	<-r.opened
	if r.m.Err() != nil {
		t.Fatalf("the channel ended: %v", r.m.Err())
	}
	// Data after CLOSE is a violation of that stream.
	r.write(fr(tClose, 3), fr(tData, 3, 1))
	waitFor(t, func() bool { return r.sent(tReset, 3) })
}

func TestHelloSignalsReadiness(t *testing.T) {
	r := newRaw(t, Config{})
	select {
	case <-r.m.Hello():
		t.Fatal("hello before it was sent")
	default:
	}
	r.write(fr(tHello, 0, Version))
	select {
	case <-r.m.Hello():
	case <-time.After(3 * time.Second):
		t.Fatal("no hello")
	}
}

func TestWriteQueueOverflowEndsTheChannel(t *testing.T) {
	// A peer that never reads: the writer goroutine blocks on the first frame and the queue fills. The channel
	// then fails instead of growing without bound.
	x, y := net.Pipe()
	defer y.Close()
	m := New(x, Config{Window: 1 << 10, MaxStreams: 1})
	defer m.Close()
	for i := 0; i < 1<<20; i++ {
		if m.send(tReset, 5, nil) != nil {
			return
		}
	}
	t.Fatal("the queue never overflowed")
}

// ---- Listener ----

func TestListener(t *testing.T) {
	l := NewListener("t", 2)
	c1, c2 := net.Pipe()
	defer c2.Close()
	if !l.Offer(c1) {
		t.Fatal("offer")
	}
	got, err := l.Accept()
	if err != nil || got != c1 {
		t.Fatalf("accept: %v", err)
	}
	// A full backlog refuses.
	var held []net.Conn
	for i := 0; i < 2; i++ {
		a, b := net.Pipe()
		defer b.Close()
		held = append(held, a)
		if !l.Offer(a) {
			t.Fatal("offer in backlog")
		}
	}
	x, y := net.Pipe()
	defer x.Close()
	defer y.Close()
	if l.Offer(x) {
		t.Fatal("offer beyond the backlog")
	}
	// Close closes what nobody accepted, ends Accept, and refuses offers.
	l.Close()
	l.Close()
	if _, err := held[0].Write([]byte("x")); err == nil {
		t.Fatal("an unaccepted connection stayed open")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("accept after close: %v", err)
	}
	if l.Offer(x) {
		t.Fatal("offer after close")
	}
	if l.Addr().Network() != "netpipe" {
		t.Fatal(l.Addr())
	}
}

func TestListenerAcceptWakesOnClose(t *testing.T) {
	l := NewListener("t", 1)
	errc := make(chan error, 1)
	go func() { _, err := l.Accept(); errc <- err }()
	time.Sleep(30 * time.Millisecond)
	l.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept stayed blocked")
	}
}
