package netpipe

import (
	"net"
	"sync"
)

// Listener is a net.Listener fed by streams instead of a socket: a proxy or an HTTP server serves it as it
// would a TCP listener, and nothing listens on the Mac.
type Listener struct {
	name string
	ch   chan net.Conn

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// NewListener makes a listener that keeps up to backlog streams waiting for Accept.
func NewListener(name string, backlog int) *Listener {
	if backlog < 1 {
		backlog = 16
	}
	return &Listener{name: name, ch: make(chan net.Conn, backlog), done: make(chan struct{})}
}

// Offer hands a connection to Accept without blocking; false when the listener is closed or its backlog is full
// (the caller resets the stream).
func (l *Listener) Offer(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	select {
	case l.ch <- c:
		return true
	default:
		return false
	}
}

// Accept implements net.Listener.
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case <-l.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops Accept and closes the connections nobody accepted.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.done)
	l.mu.Unlock()
	for {
		select {
		case c := <-l.ch:
			c.Close()
		default:
			return nil
		}
	}
}

// Addr implements net.Listener.
func (l *Listener) Addr() net.Addr { return addr{"netpipe:" + l.name} }
