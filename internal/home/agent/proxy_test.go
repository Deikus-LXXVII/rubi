package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstream is a fake server the proxy's Dial reaches instead of the Internet: it echoes with a prefix.
type upstream struct {
	mu    sync.Mutex
	addrs []string
	ln    net.Listener
}

func newUpstream(t *testing.T) *upstream {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					fmt.Fprintf(c, "echo:%s\n", sc.Text())
				}
			}()
		}
	}()
	return u
}

func publicResolve(ctx context.Context, host string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("203.0.113.7")}, nil
}

func (u *upstream) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	u.mu.Lock()
	u.addrs = append(u.addrs, addr)
	u.mu.Unlock()
	return net.Dial("tcp", u.ln.Addr().String())
}

func (u *upstream) dialed() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.addrs...)
}

const testToken = "0123456789abcdef0123456789abcdef"

func connect(t *testing.T, proxy, target string) (net.Conn, *bufio.Reader, string) {
	return connectAuth(t, proxy, target, "Proxy-Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("rubi:"+testToken))+"\r\n")
}

func connectAuth(t *testing.T, proxy, target, authHeader string) (net.Conn, *bufio.Reader, string) {
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, authHeader)
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	return c, br, strings.TrimSpace(status)
}

func TestProxyAllowDenyLog(t *testing.T) {
	up := newUpstream(t)
	var logged []string
	var lmu sync.Mutex
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: AllowList([]string{"git.example.com", "downloads.claude.ai", "intake.datadoghq.com"}, []string{"github.com"}),
		Resolve: publicResolve, Dial: up.dial, Logf: func(f string, a ...any) { lmu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); lmu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	if !strings.HasPrefix(px.Addr(), "127.0.0.1:") {
		t.Fatalf("addr %s", px.Addr())
	}
	for _, h := range []string{"Registry.NPMJS.org", "claude.ai", "console.anthropic.com", "platform.claude.com",
		"registry.npmjs.org", "pypi.org", "files.pythonhosted.org", "git.example.com", "github.com"} {
		c, br, st := connect(t, px.Addr(), h+":443")
		if !strings.Contains(st, "200") {
			t.Fatalf("%s: %s", h, st)
		}
		br.ReadString('\n') // blank line
		fmt.Fprintf(c, "ping\n")
		if line, _ := br.ReadString('\n'); strings.TrimSpace(line) != "echo:ping" {
			t.Fatalf("%s: tunnel: %q", h, line)
		}
	}
	if d := up.dialed(); len(d) != 9 || d[0] != "203.0.113.7:443" {
		t.Fatalf("dialed %v", d)
	}
	before := len(up.dialed())
	for _, h := range []string{"http-intake.logs.us5.datadoghq.com:443", "intake.datadoghq.com:443", "downloads.claude.ai:443",
		"evil.example.org:443", "api.anthropic.com:80", "api.anthropic.com:22", "93.184.216.34:443", "[::1]:443", "api.anthropic.com.evil.org:443",
		"anthropic.com:443", "sub.api.anthropic.com:443", "localhost:443"} {
		_, _, st := connect(t, px.Addr(), h)
		if !strings.Contains(st, "403") && !strings.Contains(st, "400") {
			t.Errorf("%s: %s", h, st)
		}
	}
	// A plain proxy request is refused too.
	c, _ := net.Dial("tcp", px.Addr())
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET http://api.anthropic.com/ HTTP/1.1\r\nHost: api.anthropic.com\r\nProxy-Authorization: Basic "+base64.StdEncoding.EncodeToString([]byte("rubi:"+testToken))+"\r\n\r\n")
	if st, _ := bufio.NewReader(c).ReadString('\n'); !strings.Contains(st, "405") {
		t.Errorf("plain GET: %s", st)
	}
	c.Close()
	if len(up.dialed()) != before {
		t.Fatalf("a refused request reached upstream: %v", up.dialed()[before:])
	}
	refused, opened := px.Refused()
	if opened != 9 || len(refused) != 13 {
		t.Fatalf("refused %v opened %d", refused, opened)
	}
	lmu.Lock()
	defer lmu.Unlock()
	if len(logged) != 13 || !strings.Contains(logged[0], "datadoghq.com") {
		t.Fatalf("log: %v", logged)
	}
}

func TestProxyUpstreamFailureAndClose(t *testing.T) {
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: []string{"api.anthropic.com"}, Resolve: publicResolve,
		Dial: func(ctx context.Context, n, a string) (net.Conn, error) { return nil, io.ErrClosedPipe }})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, st := connect(t, px.Addr(), "api.anthropic.com:443"); !strings.Contains(st, "502") {
		t.Fatal(st)
	}
	addr := px.Addr()
	px.Close()
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("proxy still listening")
	}
}

// The model API isn't reachable through the CONNECT proxy: the runner talks to it through the auth gateway.
func TestTheModelAPIIsNotOnTheProxyList(t *testing.T) {
	for _, h := range AllowList([]string{"git.example.com"}, ForgeHosts) {
		if h == "api.anthropic.com" {
			t.Fatal("api.anthropic.com is on the project allow-list")
		}
	}
}

func TestAllowListNeverHasRefusedHosts(t *testing.T) {
	l := AllowList([]string{"Git.Example.com.", "downloads.claude.ai", "x.datadoghq.eu", "10.0.0.1", "bad host", ""}, nil)
	want := []string{"claude.ai", "console.anthropic.com", "files.pythonhosted.org", "git.example.com",
		"platform.claude.com", "pypi.org", "registry.npmjs.org"}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("%v", l)
	}
}

func TestProxyRequiresAuthAndRefusesWildcard(t *testing.T) {
	up := newUpstream(t)
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: []string{"api.anthropic.com"}, Resolve: publicResolve, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	for _, hdr := range []string{"", "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("rubi:wrong-wrong-wrong-wrong")) + "\r\n",
		"Proxy-Authorization: Bearer " + testToken + "\r\n", "Proxy-Authorization: Basic !!!\r\n"} {
		if _, _, st := connectAuth(t, px.Addr(), "api.anthropic.com:443", hdr); !strings.Contains(st, "407") {
			t.Errorf("%q: %s", hdr, st)
		}
	}
	if len(up.dialed()) != 0 {
		t.Fatal("an unauthenticated request reached upstream")
	}
	for _, l := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		if p, err := StartProxy(ProxyConfig{Listen: l, AuthToken: testToken}); err == nil {
			p.Close()
			t.Errorf("listened on %s", l)
		}
	}
	if p, err := StartProxy(ProxyConfig{AuthToken: "short"}); err == nil {
		p.Close()
		t.Error("weak token accepted")
	}
}

func TestProxyRefusesNonPublicAddresses(t *testing.T) {
	up := newUpstream(t)
	answers := map[string][]string{
		"api.anthropic.com":      {"203.0.113.7"}, // not on the list any more: refused before it is resolved
		"claude.ai":              {"127.0.0.1"},
		"pypi.org":               {"10.1.2.3"},
		"registry.npmjs.org":     {"169.254.169.254"},
		"files.pythonhosted.org": {"203.0.113.7", "192.168.1.5"}, // one bad address is enough
		"github.com":             {"::ffff:10.0.0.1"},
		"gitlab.com":             {"fd00::1"},
		"bitbucket.org":          {"100.64.1.1"},
		"console.anthropic.com":  {"0.0.0.0"},
		"platform.claude.com":    {"224.0.0.1"},
		"git.example.com":        {"10.0.0.8"}, // the owner's own server: exempt
	}
	resolve := func(ctx context.Context, host string) ([]net.IP, error) {
		var out []net.IP
		for _, a := range answers[host] {
			out = append(out, net.ParseIP(a))
		}
		return out, nil
	}
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: AllowList([]string{"git.example.com"}, ForgeHosts),
		Exempt: []string{"git.example.com"}, Resolve: resolve, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	for h, a := range answers {
		_, _, st := connect(t, px.Addr(), h+":443")
		want := "403"
		if h == "git.example.com" {
			want = "200"
		}
		if !strings.Contains(st, want) {
			t.Errorf("%s -> %v: %s", h, a, st)
		}
	}
	d := up.dialed()
	sort.Strings(d)
	if !reflect.DeepEqual(d, []string{"10.0.0.8:443"}) {
		t.Fatalf("dialed %v", d)
	}
}

func TestBlockedAddr(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.0.0.1", "172.16.5.5", "192.168.0.1", "169.254.1.1", "fe80::1", "fd12::1", "fc00::1",
		"0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "ff02::1", "100.64.0.1", "100.127.255.255", "::ffff:10.0.0.1", "::ffff:127.0.0.1",
		"255.255.255.255", "240.0.0.1", "64:ff9b::a00:1", "2002:a00:1::"} {
		if !blockedAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s not blocked", a)
		}
	}
	for _, a := range []string{"8.8.8.8", "203.0.113.7", "160.79.104.10", "100.63.255.255", "100.128.0.1", "2606:4700::1111", "::ffff:8.8.8.8"} {
		if blockedAddr(netip.MustParseAddr(a)) {
			t.Errorf("%s blocked", a)
		}
	}
}

// The real dialer's Control refuses the address the socket would connect to, whatever the name said.
func TestGuardedDialerRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c, err := defaultDial(true)(context.Background(), "tcp", ln.Addr().String()); err == nil {
		c.Close()
		t.Fatal("guarded dialer connected to loopback")
	} else if !errors.Is(err, errBlockedAddr) {
		t.Fatalf("%v", err)
	}
	c, err := defaultDial(false)(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("exempt dialer: %v", err)
	}
	c.Close()
}

func TestProxyNetAsk(t *testing.T) {
	up := newUpstream(t)
	var asked []string
	var amu sync.Mutex
	answer := true
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: []string{"api.anthropic.com"}, Resolve: publicResolve, Dial: up.dial,
		Ask: func(ctx context.Context, host string) bool {
			amu.Lock()
			defer amu.Unlock()
			asked = append(asked, host)
			return answer
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	if _, _, st := connect(t, px.Addr(), "packages.example.net:443"); !strings.Contains(st, "200") {
		t.Fatalf("approved host: %s", st)
	}
	if _, _, st := connect(t, px.Addr(), "packages.example.net:443"); !strings.Contains(st, "200") {
		t.Fatalf("approved host again: %s", st)
	}
	answer = false
	if _, _, st := connect(t, px.Addr(), "other.example.net:443"); !strings.Contains(st, "403") {
		t.Fatalf("denied host: %s", st)
	}
	// Never asked for: other ports, refused hosts, IP literals.
	for _, h := range []string{"x.example.net:80", "downloads.claude.ai:443", "1.2.3.4:443"} {
		connect(t, px.Addr(), h)
	}
	amu.Lock()
	defer amu.Unlock()
	if !reflect.DeepEqual(asked, []string{"packages.example.net", "other.example.net"}) {
		t.Fatalf("asked %v", asked)
	}
}

func TestProxyIdleTunnelEnds(t *testing.T) {
	up := newUpstream(t)
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: []string{"api.anthropic.com"}, Resolve: publicResolve, Dial: up.dial,
		IdleTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	c, br, _ := connect(t, px.Addr(), "api.anthropic.com:443")
	br.ReadString('\n')
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("idle tunnel stayed open")
	}
}

func TestProxySetHostsRevokes(t *testing.T) {
	up := newUpstream(t)
	px, err := StartProxy(ProxyConfig{AuthToken: testToken, Hosts: []string{"a.example.com", "b.example.com"}, Resolve: publicResolve, Dial: up.dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { px.Close() })
	ca, bra, status := connect(t, px.Addr(), "a.example.com:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("a: %s", status)
	}
	cb, brb, status := connect(t, px.Addr(), "b.example.com:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("b: %s", status)
	}
	bra.ReadString('\n') // the blank line that ends the reply
	brb.ReadString('\n')
	px.SetHosts([]string{"b.example.com", "c.example.com"})
	if px.Allowed("a.example.com") || !px.Allowed("b.example.com") || !px.Allowed("c.example.com") {
		t.Fatal("the list wasn't replaced")
	}
	// a's tunnel is closed, b's stays up.
	fmt.Fprintf(ca, "x\n")
	if _, err := bra.ReadString('\n'); err == nil {
		t.Error("a revoked host's tunnel still works")
	}
	fmt.Fprintf(cb, "y\n")
	if line, err := brb.ReadString('\n'); err != nil || line != "echo:y\n" {
		t.Errorf("b's tunnel: %q %v", line, err)
	}
	if _, _, status := connect(t, px.Addr(), "a.example.com:443"); !strings.Contains(status, "403") {
		t.Errorf("a again: %s", status)
	}
}
