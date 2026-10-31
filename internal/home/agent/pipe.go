package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// A VM started with --network none has loopback and nothing else. Its way out is rubi-netpipe, a forwarder in the
// image that Rubi Home runs with `container exec -i` and keeps running: every TCP connection the runner makes to
// 127.0.0.1:3128 (the CONNECT proxy) or 127.0.0.1:3129 (the auth gateway) becomes a stream over that process's
// stdio, and Home hands the stream, as a net.Conn, to the proxy or the gateway (netpipe.Listener). Nothing listens
// on the Mac for the VM.

// pipeBin is the forwarder's place in the image (runner/container/Dockerfile).
const pipeBin = "/usr/local/bin/rubi-netpipe"

// Vars, so that tests can shorten them.
var (
	// pipeHelloWait is how long a (re)start of the forwarder may take to listen.
	pipeHelloWait = 20 * time.Second
	// pipeMaxRestarts in pipeRestartWindow: more than that and the VM is stopped (its sessions end, a start brings a fresh one).
	pipeMaxRestarts   = 3
	pipeRestartWindow = 5 * time.Minute
	pipeRestartDelay  = 500 * time.Millisecond
)

// pipeSup runs the forwarder for one container and restarts it when it dies.
type pipeSup struct {
	a      *AppleContainer
	name   string
	spec   *PipeSpec
	giveUp func(error)

	// the limits as they were when the pipe started
	helloWait   time.Duration
	maxRestarts int
	window      time.Duration
	delay       time.Duration

	mu       sync.Mutex
	mux      *netpipe.Mux
	stopped  bool
	stopCh   chan struct{}
	restarts []time.Time
}

// startPipe starts the forwarder in the running container and waits until it listens. A later death of the
// forwarder restarts it; after pipeMaxRestarts in pipeRestartWindow the container is stopped.
func (a *AppleContainer) startPipe(ctx context.Context, name string, spec *PipeSpec, giveUp func()) (*pipeSup, error) {
	s := &pipeSup{a: a, name: name, spec: spec, stopCh: make(chan struct{}),
		helloWait: pipeHelloWait, maxRestarts: pipeMaxRestarts, window: pipeRestartWindow, delay: pipeRestartDelay}
	s.giveUp = func(err error) {
		log.Printf("agent: the network pipe of %s keeps failing (%v); stopping its VM", name, err)
		giveUp()
	}
	if err := s.attempt(ctx); err != nil {
		s.stop()
		return nil, err
	}
	go s.watch()
	return s, nil
}

// attempt starts the forwarder once and returns when it said HELLO.
func (s *pipeSup) attempt(ctx context.Context) error {
	conn, err := s.a.execNamed(ctx, s.name, nil, []string{pipeBin}, true)
	if err != nil {
		return err
	}
	m := netpipe.New(conn, netpipe.Config{OnOpen: s.open, Logf: log.Printf})
	go m.Run()
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		m.Close()
		return errors.New("stopped")
	}
	s.mux = m
	s.mu.Unlock()
	t := time.NewTimer(s.helloWait)
	defer t.Stop()
	select {
	case <-m.Hello():
		return nil
	case <-m.Done():
		return fmt.Errorf("the forwarder ended before it listened: %w", m.Err())
	case <-conn.Done():
		m.Close()
		return errors.New("the forwarder exited before it listened")
	case <-ctx.Done():
		m.Close()
		return ctx.Err()
	case <-t.C:
		m.Close()
		return errors.New("the forwarder didn't listen in time")
	}
}

// open hands a new stream to the service's listener; false resets it.
func (s *pipeSup) open(svc byte, st *netpipe.Stream) bool {
	var ln *netpipe.Listener
	switch svc {
	case netpipe.SvcProxy:
		ln = s.spec.Proxy
	case netpipe.SvcGateway:
		ln = s.spec.Gateway
	}
	return ln != nil && ln.Offer(st)
}

func (s *pipeSup) watch() {
	for {
		s.mu.Lock()
		m := s.mux
		s.mu.Unlock()
		if m != nil {
			select {
			case <-m.Done():
			case <-s.stopCh:
				return
			}
		}
		for { // restart until it listens again, or give up
			now := time.Now()
			s.mu.Lock()
			if s.stopped {
				s.mu.Unlock()
				return
			}
			kept := s.restarts[:0]
			for _, t := range s.restarts {
				if now.Sub(t) < s.window {
					kept = append(kept, t)
				}
			}
			s.restarts = kept
			var last error
			if m != nil {
				last = m.Err()
			}
			if len(s.restarts) >= s.maxRestarts {
				s.mu.Unlock()
				s.giveUp(fmt.Errorf("%d restarts in %s: %v", len(s.restarts), s.window, last))
				return
			}
			s.restarts = append(s.restarts, now)
			delay := s.delay << (len(s.restarts) - 1)
			s.mu.Unlock()
			log.Printf("agent: the network pipe of %s ended (%v); restarting it", s.name, last)
			select {
			case <-time.After(delay):
			case <-s.stopCh:
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.helloWait+5*time.Second)
			err := s.attempt(ctx)
			cancel()
			if err == nil {
				break
			}
			s.mu.Lock()
			m = s.mux
			s.mu.Unlock()
			if m != nil {
				m.Close()
			}
			log.Printf("agent: restarting the network pipe of %s failed: %v", s.name, err)
		}
	}
}

// stop ends the forwarder for good (safe on nil and twice).
func (s *pipeSup) stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)
	m := s.mux
	s.mu.Unlock()
	if m != nil {
		m.Close()
	}
}
