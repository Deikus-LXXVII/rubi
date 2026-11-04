package guest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// fakeGuest serves the channel with exec streams handled by behave (after reading the request), the way a guest that
// has been taken over might.
func fakeGuest(t *testing.T, behave func(st *netpipe.Stream)) *Client {
	t.Helper()
	a, b := net.Pipe()
	m := netpipe.New(a, netpipe.Config{OnOpen: func(svc byte, st *netpipe.Stream) bool {
		go func() {
			if _, err := readLine(st); err != nil {
				return
			}
			behave(st)
		}()
		return true
	}})
	go func() { _ = m.Run() }()
	_ = m.SendHello()
	c := NewClient(b, nil)
	t.Cleanup(func() { _ = c.Close(); _ = m.Close() })
	if err := c.Ready(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	return c
}

func started(st *netpipe.Stream) {
	b, _ := json.Marshal(ExecStarted{})
	_, _ = st.Write(append(b, '\n'))
}

// Whatever the guest does, Home gets an error and the process's end, never a hang, a panic or an unbounded read.
func TestHostileGuest(t *testing.T) {
	for name, c := range map[string]struct {
		behave  func(st *netpipe.Stream)
		wantErr error  // from Wait (nil: any error)
		inExec  string // the error comes from Exec itself, containing this
	}{
		"a chunk kind of Home's": {behave: func(st *netpipe.Stream) { started(st); _ = writeChunk(st, ChunkStdin, []byte("x")) }, wantErr: ErrProtocol},
		"an unknown chunk kind":  {behave: func(st *netpipe.Stream) { started(st); _ = writeChunk(st, 99, nil) }, wantErr: ErrProtocol},
		"a short exit status":    {behave: func(st *netpipe.Stream) { started(st); _ = writeChunk(st, ChunkExit, []byte{0, 0, 0}) }, wantErr: ErrProtocol},
		"an oversize chunk": {behave: func(st *netpipe.Stream) {
			started(st)
			_, _ = st.Write([]byte{ChunkStdout, 0xff, 0xff, 0xff, 0xff})
		}, wantErr: ErrProtocol},
		"no exit status": {behave: func(st *netpipe.Stream) {
			started(st)
			_ = writeChunk(st, ChunkStdout, []byte("hi"))
			_ = st.CloseWrite()
		}},
		"a stream cut short": {behave: func(st *netpipe.Stream) {
			started(st)
			_, _ = st.Write([]byte{ChunkStdout, 0, 0, 0, 9, 'a'})
			_ = st.CloseWrite()
		}},
		"an overlong answer":   {behave: func(st *netpipe.Stream) { _, _ = st.Write([]byte(strings.Repeat("x", MaxLine+10))) }, inExec: "protocol"},
		"an answer that isn't": {behave: func(st *netpipe.Stream) { _, _ = st.Write([]byte("[1,2]\n")) }, inExec: "protocol"},
		"a refusal": {behave: func(st *netpipe.Stream) {
			b, _ := json.Marshal(ExecStarted{Error: "no"})
			_, _ = st.Write(append(b, '\n'))
		}, inExec: ": no"},
	} {
		t.Run(name, func(t *testing.T) {
			cl := fakeGuest(t, c.behave)
			p, err := cl.Exec(ctx5(t), sh("true"), nil)
			if c.inExec != "" {
				if err == nil || !strings.Contains(err.Error(), c.inExec) {
					t.Fatalf("Exec: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			go func() { _, _ = io.Copy(io.Discard, p) }()
			select {
			case <-p.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("no end")
			}
			code, err := p.Wait()
			if err == nil || code != -1 || (c.wantErr != nil && !errors.Is(err, c.wantErr)) {
				t.Fatalf("%d %v", code, err)
			}
		})
	}
}

// A guest that never answers holds Home only as long as the caller's deadline.
func TestSilentGuestHoldsOnlyUntilTheDeadline(t *testing.T) {
	cl := fakeGuest(t, func(st *netpipe.Stream) { select {} })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := cl.Exec(ctx, sh("true"), nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

// What the client refuses before asking the guest: no program, root, or an id that would wrap to root.
func TestClientRefusesBadRequests(t *testing.T) {
	cl := fakeGuest(t, func(st *netpipe.Stream) { t.Error("the guest was asked") })
	for name, req := range map[string]ExecRequest{
		"no program":      {Dir: "/", UID: 1000, GID: 1000},
		"root":            {Argv: []string{"/bin/true"}, Dir: "/", UID: 0, GID: 1000},
		"root group":      {Argv: []string{"/bin/true"}, Dir: "/", UID: 1000, GID: 0},
		"wraps to root":   {Argv: []string{"/bin/true"}, Dir: "/", UID: 1 << 32, GID: 1000},
		"the no-id value": {Argv: []string{"/bin/true"}, Dir: "/", UID: 1<<32 - 1, GID: 1000},
		"negative":        {Argv: []string{"/bin/true"}, Dir: "/", UID: -5, GID: 1000},
	} {
		if _, err := cl.Exec(ctx5(t), req, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
		if _, err := command(req); err == nil {
			t.Errorf("%s accepted by the guest", name)
		}
	}
}
