package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// vmPipes plays the VM's end of `container exec -i ... rubi-netpipe`: a spawn seam that, for the forwarder's command,
// returns a Conn on one end of an in-memory duplex and runs a netpipe.Mux (the forwarder's side: it says HELLO and
// can open streams) on the other. Any other command goes to inner.
type vmPipes struct {
	t       *testing.T
	inner   func(ctx context.Context, argv []string) (Conn, error)
	NoHello bool // the forwarder never listens

	mu     sync.Mutex
	starts int
	muxes  []*netpipe.Mux
	conns  []*pipeEnd
	argvs  [][]string
}

func newVMPipes(t *testing.T, inner func(ctx context.Context, argv []string) (Conn, error)) *vmPipes {
	if inner == nil {
		inner = func(context.Context, []string) (Conn, error) { return nopConn{}, nil }
	}
	return &vmPipes{t: t, inner: inner}
}

func (v *vmPipes) spawn(ctx context.Context, argv []string) (Conn, error) {
	if argv[len(argv)-1] != pipeBin {
		return v.inner(ctx, argv)
	}
	x, y := net.Pipe()
	end := &pipeEnd{Conn: x, done: make(chan struct{})}
	m := netpipe.New(y, netpipe.Config{})
	go m.Run()
	if !v.NoHello {
		m.SendHello()
	}
	v.mu.Lock()
	v.starts++
	v.muxes = append(v.muxes, m)
	v.conns = append(v.conns, end)
	v.argvs = append(v.argvs, argv)
	v.mu.Unlock()
	v.t.Cleanup(func() { m.Close(); end.Close() })
	return end, nil
}

func (v *vmPipes) latest() *netpipe.Mux {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.muxes[len(v.muxes)-1]
}

func (v *vmPipes) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.starts
}

// kill ends the latest forwarder as if its exec process died.
func (v *vmPipes) kill() {
	v.mu.Lock()
	c := v.conns[len(v.conns)-1]
	v.mu.Unlock()
	c.Close()
}

// pipeEnd is the Conn of the exec process on the Mac's side.
type pipeEnd struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (p *pipeEnd) CloseWrite() error     { return p.Conn.Close() }
func (p *pipeEnd) Done() <-chan struct{} { return p.done }
func (p *pipeEnd) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.Conn.Close()
}

// fastPipe shortens the pipe's timers for a test.
func fastPipe(t *testing.T) {
	ow, om, oc, od := pipeHelloWait, pipeMaxRestarts, pipeRestartWindow, pipeRestartDelay
	pipeHelloWait, pipeRestartDelay = 500*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pipeHelloWait, pipeMaxRestarts, pipeRestartWindow, pipeRestartDelay = ow, om, oc, od })
}

// connectVia opens a stream to the service from the VM side, writes request and returns the first line of the answer.
func connectVia(t *testing.T, m *netpipe.Mux, svc byte, request string) string {
	t.Helper()
	s, err := m.Open(svc)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Write([]byte(request)); err != nil {
		return "write: " + err.Error()
	}
	line, err := bufio.NewReader(s).ReadString('\n')
	if err != nil {
		return "read: " + err.Error()
	}
	return strings.TrimSpace(line)
}

func basic(tok string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("rubi:"+tok))
}

// startNetless starts a project on an AppleContainer whose container CLI is a fake, with the forwarder played by vm.
func startNetless(t *testing.T, vm *vmPipes, spec StartSpec) (*AppleContainer, *authFake, Conn, error) {
	t.Helper()
	f := &authFake{}
	a, _ := newAuthVM(t, f)
	a.spawn = vm.spawn
	if spec.Project.Dir == "" {
		spec.Project = Project{ID: "my-project", Dir: "/home/alice/my-project"}
	}
	c, err := a.Start(context.Background(), spec)
	return a, f, c, err
}

func TestNetlessStartRunsWithoutNetworkAndServesStreams(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, nil)
	pl, gl := netpipe.NewListener("proxy", 4), netpipe.NewListener("gateway", 4)
	px, err := StartProxy(ProxyConfig{Listener: pl, AuthToken: "0123456789abcdef", Hosts: []string{"registry.npmjs.org"},
		Resolve: func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
		Dial: func(ctx context.Context, n, addr string) (net.Conn, error) {
			c, _ := net.Pipe()
			return c, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	gw, err := StartGateway(GatewayConfig{Listener: gl, Secret: strings.Repeat("s", 40), TokenFile: t.TempDir() + "/none"})
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	a, f, c, err := startNetless(t, vm, StartSpec{Env: map[string]string{"A": "1"}, Pipe: &PipeSpec{Proxy: pl, Gateway: gl}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	defer c.Close()
	// no network command of any kind; the container has no network
	for _, cmd := range f.cmds {
		if len(cmd) > 1 && cmd[1] == "network" {
			t.Errorf("a network command ran: %v", cmd)
		}
	}
	run := strings.Join(f.find("run -d"), " ")
	if !strings.Contains(run, "--network none") || strings.Contains(run, "rubi-agent ") {
		t.Errorf("run: %s", run)
	}
	// the forwarder is exec'd with no environment file and no secret on its command line
	if len(vm.argvs) != 1 || vm.argvs[0][len(vm.argvs[0])-1] != pipeBin || strings.Contains(strings.Join(vm.argvs[0], " "), "--env-file") {
		t.Errorf("forwarder exec: %v", vm.argvs)
	}
	m := vm.latest()
	// the proxy: no credentials, an unlisted host, a listed host
	if got := connectVia(t, m, netpipe.SvcProxy, "CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.Contains(got, " 407 ") {
		t.Errorf("no credentials: %q", got)
	}
	if got := connectVia(t, m, netpipe.SvcProxy, "CONNECT example.org:443 HTTP/1.1\r\nHost: x\r\nProxy-Authorization: "+basic("0123456789abcdef")+"\r\n\r\n"); !strings.Contains(got, " 403 ") {
		t.Errorf("unlisted host: %q", got)
	}
	if got := connectVia(t, m, netpipe.SvcProxy, "CONNECT registry.npmjs.org:443 HTTP/1.1\r\nHost: x\r\nProxy-Authorization: "+basic("0123456789abcdef")+"\r\n\r\n"); !strings.Contains(got, " 200 ") {
		t.Errorf("listed host: %q", got)
	}
	// the gateway: a wrong secret is a 401 (the same checks as on a socket)
	if got := connectVia(t, m, netpipe.SvcGateway, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer nope\r\nContent-Length: 0\r\n\r\n"); !strings.Contains(got, " 401 ") {
		t.Errorf("gateway: %q", got)
	}
	// nothing listens on the Mac for the VM
	if px.Addr() != "netpipe:proxy" || gw.Addr() != "netpipe:gateway" {
		t.Errorf("addresses %q %q", px.Addr(), gw.Addr())
	}
}

func TestNetlessStartNeedsItsPipe(t *testing.T) {
	vm := newVMPipes(t, func(context.Context, []string) (Conn, error) { t.Error("runner started"); return nopConn{}, nil })
	a, f, _, err := startNetless(t, vm, StartSpec{})
	if err == nil || a.State() != StateFailed || f.find("run ") != nil {
		t.Fatalf("started without a pipe: %v %s %v", err, a.State(), f.cmds)
	}
}

func TestNetlessStartFailsClosedWhenTheForwarderNeverListens(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, func(context.Context, []string) (Conn, error) { t.Error("runner started"); return nopConn{}, nil })
	vm.NoHello = true
	a, f, _, err := startNetless(t, vm, StartSpec{Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}})
	if err == nil || a.State() != StateFailed {
		t.Fatalf("started: %v %s", err, a.State())
	}
	if f.find("delete --force rubi-agent-my-project") == nil {
		t.Errorf("container left: %v", f.cmds)
	}
}

func TestNetlessAStreamToAMissingServiceIsReset(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, nil)
	a, _, c, err := startNetless(t, vm, StartSpec{Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}}) // no gateway
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	defer c.Close()
	s, _ := vm.latest().Open(netpipe.SvcGateway)
	s.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, netpipe.ErrReset) {
		t.Fatalf("stream to a service nobody serves: %v", err)
	}
}

// The forwarder's process dies: its streams fail, Home starts it again, and new streams work.
func TestNetlessPipeRestartsAfterItsProcessDies(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, nil)
	pl := netpipe.NewListener("p", 4)
	a, _, c, err := startNetless(t, vm, StartSpec{Pipe: &PipeSpec{Proxy: pl}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	defer c.Close()
	if _, err := vm.latest().Open(netpipe.SvcProxy); err != nil {
		t.Fatal(err)
	}
	conn, err := pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	vm.kill()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a stream survived the death of its channel")
	}
	eventually(t, "restarted", func() bool { return vm.count() == 2 })
	if a.State() != StateRunning {
		t.Fatalf("state %s", a.State())
	}
	if _, err := vm.latest().Open(netpipe.SvcProxy); err != nil {
		t.Fatal(err)
	}
	acc := make(chan net.Conn, 1)
	go func() { c, _ := pl.Accept(); acc <- c }()
	select {
	case <-acc:
	case <-time.After(3 * time.Second):
		t.Fatal("no stream after the restart")
	}
}

// A forwarder that keeps dying stops the VM instead of leaving it without a way out.
func TestNetlessPipeThatKeepsDyingStopsTheVM(t *testing.T) {
	fastPipe(t)
	pipeMaxRestarts = 2
	vm := newVMPipes(t, nil)
	a, f, c, err := startNetless(t, vm, StartSpec{Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 1; i <= 3; i++ {
		eventually(t, "forwarder started", func() bool { return vm.count() == i })
		vm.kill()
	}
	eventually(t, "stopped", func() bool { return a.State() == StateStopped })
	if vm.count() != 3 {
		t.Errorf("the forwarder was started %d times, want 1 + 2 restarts", vm.count())
	}
	eventually(t, "container deleted", func() bool {
		return f.find("stop --time 5") != nil && f.find("delete --force rubi-agent-my-project") != nil
	})
}

func TestNetlessStopEndsTheForwarder(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, nil)
	a, _, c, err := startNetless(t, vm, StartSpec{Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.latest().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the channel stayed open after Stop")
	}
	time.Sleep(100 * time.Millisecond)
	if vm.count() != 1 {
		t.Errorf("the forwarder was restarted after Stop (%d)", vm.count())
	}
}
