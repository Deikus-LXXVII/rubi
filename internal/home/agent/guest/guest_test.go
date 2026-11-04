package guest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rig struct {
	c       *Client
	srv     *Server
	mu      sync.Mutex
	mounts  []Mount
	powered atomic.Int32
	killed  atomic.Int32
	served  chan error
}

func newRig(t *testing.T) *rig {
	t.Helper()
	a, b := net.Pipe()
	r := &rig{served: make(chan error, 1)}
	r.srv = &Server{
		Mount:    func(m Mount) error { r.mu.Lock(); defer r.mu.Unlock(); r.mounts = append(r.mounts, m); return nil },
		PowerOff: func() error { r.powered.Add(1); return nil },
		Kill:     func(cmd *exec.Cmd) { r.killed.Add(1); _ = cmd.Process.Kill() },
	}
	go func() { r.served <- r.srv.Serve(a) }()
	r.c = NewClient(b, nil)
	t.Cleanup(func() { _ = r.c.Close(); _ = a.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	return r
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var plain = map[string]string{"PATH": "/usr/bin:/bin"}

func sh(script string) ExecRequest {
	return ExecRequest{Argv: []string{"sh", "-c", script}, Env: plain, Dir: "/", UID: 1000, GID: 1000}
}

// stdin in, stdout and stderr out, kept apart, and the exit status last.
func TestExecStdioAndExitStatus(t *testing.T) {
	r := newRig(t)
	var errBuf bytes.Buffer
	p, err := r.c.Exec(ctx5(t), sh("cat; echo oops >&2; exit 3"), &errBuf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.Wait()
	if string(out) != "hello\n" || errBuf.String() != "oops\n" || code != 3 || err != nil {
		t.Fatalf("out %q, err %q, code %d, %v", out, errBuf.String(), code, err)
	}
}

// A megabyte each way arrives whole and in order (the chunks and netpipe's credit).
func TestExecCarriesLargeOutput(t *testing.T) {
	r := newRig(t)
	p, err := r.c.Exec(ctx5(t), sh("cat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("0123456789abcdef"), 1<<16)
	go func() { _, _ = p.Write(want); _ = p.CloseWrite() }()
	got, err := io.ReadAll(p)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%d bytes back of %d, %v", len(got), len(want), err)
	}
	if code, err := p.Wait(); code != 0 || err != nil {
		t.Fatalf("%d %v", code, err)
	}
}

// The process gets exactly the environment of the request: nothing of the guest agent's own.
func TestExecEnvironmentIsTheRequests(t *testing.T) {
	r := newRig(t)
	req := sh("env")
	req.Env = map[string]string{"PATH": "/usr/bin:/bin", "RUBI_X": "a b=c"}
	p, err := r.c.Exec(ctx5(t), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p)
	lines := strings.Fields(strings.ReplaceAll(string(out), " ", "_"))
	var got []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "PWD=") && !strings.HasPrefix(l, "SHLVL=") && !strings.HasPrefix(l, "_=") {
			got = append(got, l)
		}
	}
	if want := []string{"PATH=/usr/bin:/bin", "RUBI_X=a_b=c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("environment %q", got)
	}
}

// Closing the process from Home's side kills it in the guest.
func TestCloseKillsTheProcess(t *testing.T) {
	r := newRig(t)
	p, err := r.c.Exec(ctx5(t), sh("exec sleep 30"), nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = p.Close()
	deadline := time.Now().Add(5 * time.Second)
	for r.killed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.killed.Load() != 1 || time.Since(start) > 5*time.Second {
		t.Fatalf("killed %d times", r.killed.Load())
	}
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not done")
	}
}

// What the guest refuses, with the reason, and Home's own refusal of root.
func TestExecRefusals(t *testing.T) {
	r := newRig(t)
	if _, err := r.c.Exec(ctx5(t), ExecRequest{Argv: []string{"/bin/true"}, Dir: "/", UID: 0, GID: 0}, nil); err == nil {
		t.Error("root accepted by the client")
	}
	for name, c := range map[string]struct {
		req  ExecRequest
		want string
	}{
		"no program":        {ExecRequest{Dir: "/", UID: 1000, GID: 1000}, "no program"},
		"root":              {ExecRequest{Argv: []string{"/bin/true"}, Dir: "/", UID: 0, GID: 1000}, "root"},
		"relative folder":   {ExecRequest{Argv: []string{"/bin/true"}, Dir: "work", UID: 1000, GID: 1000}, "absolute"},
		"relative program":  {ExecRequest{Argv: []string{"bin/true"}, Dir: "/", UID: 1000, GID: 1000}, "absolute path or a name"},
		"not in PATH":       {ExecRequest{Argv: []string{"no-such-program-here"}, Env: plain, Dir: "/", UID: 1000, GID: 1000}, "not found"},
		"no PATH":           {ExecRequest{Argv: []string{"sh"}, Dir: "/", UID: 1000, GID: 1000}, "not found"},
		"bad variable name": {ExecRequest{Argv: []string{"/bin/true"}, Env: map[string]string{"A=B": "c"}, Dir: "/", UID: 1000, GID: 1000}, "environment"},
	} {
		if _, err := command(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// the guest's own answer reaches the client as an error
	_, err := r.c.Exec(ctx5(t), ExecRequest{Argv: []string{"no-such-program-here"}, Env: plain, Dir: "/", UID: 1000, GID: 1000}, nil)
	if err == nil || !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("%v", err)
	}
}

func TestMountPingAndPowerOff(t *testing.T) {
	r := newRig(t)
	ms := []Mount{{"rubi0", "/work", false}, {"rubi1", "/work/.rubi", true}, {"rubi2", "/rubi-role", true}}
	if err := r.c.Mount(ctx5(t), ms); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	got := append([]Mount(nil), r.mounts...)
	r.mu.Unlock()
	if !reflect.DeepEqual(got, ms) {
		t.Fatalf("mounted %+v", got)
	}
	for _, bad := range [][]Mount{{{"rubi0", "/", false}}, {{"rubi0", "work", false}}, {{"rubi0", "/work/../etc", false}}, {{"", "/work", false}}} {
		if err := r.c.Mount(ctx5(t), bad); err == nil {
			t.Errorf("%+v mounted", bad)
		}
	}
	if err := r.c.Ping(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if err := r.c.PowerOff(ctx5(t)); err != nil || r.powered.Load() != 1 {
		t.Fatalf("%v, powered off %d times", err, r.powered.Load())
	}
}

// A guest that breaks the protocol gives Home an error, never a hang or a huge read.
func TestClientBoundsWhatTheGuestSends(t *testing.T) {
	var b bytes.Buffer
	h := make([]byte, 5)
	h[0] = ChunkStdout
	binary.BigEndian.PutUint32(h[1:], MaxChunk+1)
	b.Write(h)
	if _, _, err := readChunk(&b); !errors.Is(err, ErrProtocol) {
		t.Errorf("an oversize chunk: %v", err)
	}
	long := strings.Repeat("x", MaxLine+1) + "\n"
	if _, err := readLine(strings.NewReader(long)); !errors.Is(err, ErrProtocol) {
		t.Errorf("an overlong line: %v", err)
	}
	if _, err := readLine(strings.NewReader("{}")); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a line cut off: %v", err)
	}
	if err := writeChunk(io.Discard, ChunkStdin, make([]byte, MaxChunk+1)); !errors.Is(err, ErrProtocol) {
		t.Errorf("writing an oversize chunk: %v", err)
	}
}
