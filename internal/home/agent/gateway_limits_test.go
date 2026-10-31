package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func gwWith(t *testing.T, up *upstreamRec, logs *strings.Builder) (*Gateway, string) {
	t.Helper()
	dir := t.TempDir()
	tf := filepath.Join(dir, "token", "oauth-token")
	os.MkdirAll(filepath.Dir(tf), 0o700)
	os.WriteFile(tf, []byte(fakeToken), 0o600)
	var mu sync.Mutex
	g, err := StartGateway(GatewayConfig{Secret: testSecret, TokenFile: tf, upstream: up.srv.URL, client: up.srv.Client(),
		Logf: func(f string, a ...any) {
			if logs != nil {
				mu.Lock()
				fmt.Fprintf(logs, f+"\n", a...)
				mu.Unlock()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return g, tf
}

func TestGatewayCapsRequestsInFlight(t *testing.T) {
	old := gatewayMaxInFlight
	gatewayMaxInFlight = 4
	defer func() { gatewayMaxInFlight = old }()
	release := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(4)
	var once [4]sync.Once
	var n int
	var mu sync.Mutex
	up := newRecUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		if i < 4 {
			once[i].Do(arrived.Done)
			<-release
		}
		fmt.Fprint(w, "{}")
	})
	g, _ := gwWith(t, up, nil)
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+testSecret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Retry-After") + "|" + string(b)
	}
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func() { c, _ := post(); codes <- c }()
	}
	arrived.Wait()
	code, extra := post()
	if code != 429 || !strings.HasPrefix(extra, "5|") || !strings.Contains(extra, "rate_limit_error") {
		t.Errorf("the fifth request: %d %s", code, extra)
	}
	close(release)
	for i := 0; i < 4; i++ {
		if c := <-codes; c != 200 {
			t.Errorf("held request: %d", c)
		}
	}
	// the place is free again
	if code, _ := post(); code != 200 {
		t.Errorf("after: %d", code)
	}
	if g.Refusals()["too many requests"] != 1 {
		t.Errorf("refusals: %v", g.Refusals())
	}
}

func TestGatewayLogsARefusalOncePerInterval(t *testing.T) {
	var logs strings.Builder
	up := newRecUpstream(t, nil)
	g, _ := gwWith(t, up, &logs)
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if n := strings.Count(logs.String(), "refused (bad secret)"); n != 1 {
		t.Errorf("%d log lines for 200 refusals:\n%s", n, logs.String())
	}
	if g.Refusals()["bad secret"] != 200 {
		t.Errorf("the count is kept: %v", g.Refusals())
	}
	// with the interval over, the next line says how many were left out
	old := gatewayLogEvery
	gatewayLogEvery = 0
	defer func() { gatewayLogEvery = old }()
	req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader("{}"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if !strings.Contains(logs.String(), "and 199 more") {
		t.Errorf("no summary line:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "wrong") || strings.Contains(logs.String(), testSecret) {
		t.Error("a secret in the log")
	}
}

func TestGatewayTimeouts(t *testing.T) {
	up := newRecUpstream(t, nil)
	g, _ := gwWith(t, up, nil)
	if g.srv.ReadTimeout != gatewayReadBudget || g.srv.ReadHeaderTimeout == 0 || g.srv.WriteTimeout != 0 || g.srv.MaxHeaderBytes == 0 {
		t.Errorf("server: read %v header %v write %v max header %d", g.srv.ReadTimeout, g.srv.ReadHeaderTimeout, g.srv.WriteTimeout, g.srv.MaxHeaderBytes)
	}
	// a request whose body never arrives is cut off at the read budget
	old := gatewayReadBudget
	gatewayReadBudget = 300 * time.Millisecond
	defer func() { gatewayReadBudget = old }()
	g2, _ := gwWith(t, up, nil)
	c, err := net.Dial("tcp", g2.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 1000000\r\n\r\n{", testSecret)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	b, _ := io.ReadAll(c) // the server ends the connection
	if time.Since(start) > 4*time.Second {
		t.Errorf("the stalled upload held the connection for %v", time.Since(start))
	}
	if strings.Contains(string(b), " 200 ") {
		t.Errorf("answered %q", b)
	}
	// the body is streamed on, so the upstream saw the start of it; it never saw a complete request
	up.mu.Lock()
	defer up.mu.Unlock()
	for _, r := range up.reqs {
		if len(r.body) >= 1000000 {
			t.Errorf("a complete body reached the upstream")
		}
	}
}

// A guest that stops reading can't pin the handler and the upstream stream.
func TestGatewayCutsOffAGuestThatStopsReading(t *testing.T) {
	old := gatewayWriteChunk
	gatewayWriteChunk = 300 * time.Millisecond
	defer func() { gatewayWriteChunk = old }()
	ended := make(chan struct{})
	up := newRecUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		chunk := make([]byte, 1<<20)
		for i := 0; i < 400; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if r.Context().Err() != nil {
				return
			}
		}
	})
	g, _ := gwWith(t, up, nil)
	c, err := net.Dial("tcp", g.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetReadBuffer(4096)
	}
	fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 2\r\n\r\n{}", testSecret)
	select { // and the guest never reads
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("the upstream stream is still pinned by a guest that doesn't read")
	}
}

func TestListenValidationIsTheSameForTheProxyAndTheGateway(t *testing.T) {
	for _, l := range []string{"0.0.0.0:0", ":0", "[::]:0", "[::0]:0", "0:0", "nonsense", "127.0.0.1"} {
		if err := validateListen(l, "gateway"); err == nil {
			t.Errorf("%q accepted", l)
		}
		if p, err := StartProxy(ProxyConfig{Listen: l, AuthToken: strings.Repeat("a", 20)}); err == nil {
			p.Close()
			t.Errorf("proxy %q accepted", l)
		}
		if g, err := StartGateway(GatewayConfig{Listen: l, Secret: testSecret}); err == nil {
			g.Close()
			t.Errorf("gateway %q accepted", l)
		}
	}
	for _, l := range []string{"127.0.0.1:0", "[::1]:0", "192.168.64.1:0", "localhost:0"} {
		if err := validateListen(l, "gateway"); err != nil {
			t.Errorf("%q refused: %v", l, err)
		}
	}
}

func TestReadTokenOpensWithoutFollowingAndChecksTheDescriptor(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	os.WriteFile(good, []byte(fakeToken+"\n"), 0o600)
	if tok, ok := readToken(good); !ok || tok != fakeToken {
		t.Fatalf("%q %v", tok, ok)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(good, link)
	if _, ok := readToken(link); ok {
		t.Error("followed a symlink")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() { _, ok := readToken(fifo); done <- ok }()
	select {
	case ok := <-done:
		if ok {
			t.Error("a FIFO is a token")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO hangs")
	}
	os.Mkdir(filepath.Join(dir, "adir"), 0o700)
	if _, ok := readToken(filepath.Join(dir, "adir")); ok {
		t.Error("a directory is a token")
	}
	big := filepath.Join(dir, "big")
	os.WriteFile(big, []byte("sk-ant-"+strings.Repeat("a", 4100)), 0o600)
	if _, ok := readToken(big); ok {
		t.Error("an oversized file is a token")
	}
	if _, ok := readToken(filepath.Join(dir, "missing")); ok {
		t.Error("a missing file is a token")
	}
}

func TestNilGatewayAndProxyCloseDoNothing(t *testing.T) {
	var g *Gateway
	g.Close()
	var p *Proxy
	if err := p.Close(); err != nil {
		t.Error(err)
	}
}

// Connect fails before Prepare ever ran (a VM that doesn't call it): the start fails cleanly, no panic.
type noPrepareVM struct{ f *Fake }

func (v noPrepareVM) Start(ctx context.Context, spec StartSpec) (Conn, error) {
	spec.Prepare = nil
	return v.f.Start(ctx, spec)
}
func (v noPrepareVM) Exec(ctx context.Context, argv []string, env map[string]string) (Conn, error) {
	return v.f.Exec(ctx, argv, env)
}
func (v noPrepareVM) Stop(ctx context.Context) error { return v.f.Stop(ctx) }
func (v noPrepareVM) State() State                   { return v.f.State() }

func TestAStartThatFailsBeforePrepareDoesNotPanic(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		f := &Fake{Setup: func(fr *FakeRunner) { fr.BadToken = true }}
		o.NewVM = func(Project) VM { return noPrepareVM{f} }
	})
	if _, err := e.m.Sandbox(e.ctx, "my-project"); err == nil {
		t.Error("a start with a bad hello succeeded")
	}
}

func TestMultipleBetaLinesAreAllKept(t *testing.T) {
	up := newRecUpstream(t, nil)
	g, _ := gwWith(t, up, nil)
	do := func(lines ...string) string {
		req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+testSecret)
		for _, l := range lines {
			req.Header.Add("Anthropic-Beta", l)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		up.mu.Lock()
		defer up.mu.Unlock()
		return strings.Join(up.reqs[len(up.reqs)-1].header.Values("Anthropic-Beta"), "|")
	}
	if got := do("claude-code-20250219", "interleaved-thinking-2025-05-14"); got != "claude-code-20250219,interleaved-thinking-2025-05-14,oauth-2025-04-20" {
		t.Errorf("two lines: %q", got)
	}
	if got := do("claude-code-20250219", "oauth-2025-04-20"); got != "claude-code-20250219,oauth-2025-04-20" {
		t.Errorf("oauth on the second line: %q", got)
	}
	if got := do(); got != "oauth-2025-04-20" {
		t.Errorf("none: %q", got)
	}
	// whichever beta the agent asks for is passed on: the gateway injects a credential, it is no policy on features
	if got := do("some-future-beta-2030-01-01"); !strings.Contains(got, "some-future-beta-2030-01-01") {
		t.Errorf("a beta was dropped: %q", got)
	}
}

// The lock on api.anthropic.com can't be turned off by configuration: the override fields are unexported and
// the hook that sets them exists only in a test build.
func TestTheUpstreamLockHasNoProductionSwitch(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(GatewayConfig{}), reflect.TypeOf(Options{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.IsExported() && (strings.Contains(strings.ToLower(f.Name), "upstream") || f.Type == reflect.TypeOf((*http.Client)(nil))) {
				t.Errorf("%s.%s can override the gateway's upstream", typ.Name(), f.Name)
			}
		}
	}
	if _, err := StartGateway(GatewayConfig{Secret: testSecret, upstream: "https://example.com"}); err == nil {
		t.Error("a foreign upstream without the test client was accepted")
	}
}
