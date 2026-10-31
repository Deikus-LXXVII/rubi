package netpipe

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type cmdRW struct {
	io.Reader
	io.WriteCloser
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

// TestInteropWithTheNodeForwarder runs the real runner/container/rubi-netpipe (when node is installed) against
// this package's Mux: the two implementations must agree on framing, windows and half closes.
func TestInteropWithTheNodeForwarder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	script, _ := filepath.Abs("../../../../runner/container/rubi-netpipe")
	if _, err := os.Stat(script); err != nil {
		t.Skip("no forwarder script")
	}
	pp, gp := freePort(t), freePort(t)
	cmd := exec.Command(node, script, "--proxy-port", pp, "--gateway-port", gp)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })

	got := make(map[byte]int)
	var mu sync.Mutex
	m := New(cmdRW{out, in}, Config{OnOpen: func(svc byte, s *Stream) bool {
		mu.Lock()
		got[svc]++
		mu.Unlock()
		if svc == SvcGateway { // the gateway answers a request after the client half-closes, then closes
			go func() {
				b, _ := io.ReadAll(s)
				s.Write(append([]byte("echo:"), b...))
				s.CloseWrite()
				s.Close()
			}()
			return true
		}
		return echo(svc, s)
	}})
	go m.Run()
	t.Cleanup(func() { m.Close() })
	select {
	case <-m.Hello():
	case <-time.After(10 * time.Second):
		t.Fatal("no HELLO from the forwarder")
	}

	// 6 MiB through the proxy stream (many windows) and back, byte for byte.
	c, err := net.Dial("tcp", "127.0.0.1:"+pp)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	data := rnd(6<<20 + 3)
	go func() { c.Write(data); c.(*net.TCPConn).CloseWrite() }()
	var back []byte
	within(t, "echo through node", 30*time.Second, func() { back, _ = io.ReadAll(c) })
	if !bytes.Equal(back, data) {
		t.Fatalf("got %d bytes back of %d", len(back), len(data))
	}

	// Half close: the request ends with the client's FIN, the answer still arrives.
	g, err := net.Dial("tcp", "127.0.0.1:"+gp)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Write([]byte("hello"))
	g.(*net.TCPConn).CloseWrite()
	within(t, "gateway answer", 10*time.Second, func() { back, _ = io.ReadAll(g) })
	if string(back) != "echo:hello" {
		t.Fatalf("gateway answer %q", back)
	}
	mu.Lock()
	defer mu.Unlock()
	if got[SvcProxy] != 1 || got[SvcGateway] != 1 {
		t.Fatalf("services seen: %v", got)
	}

	// The channel ends: the forwarder exits 0.
	m.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwarder exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarder didn't exit when the channel closed")
	}
}
