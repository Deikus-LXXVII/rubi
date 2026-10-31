package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
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

func connect(t *testing.T, proxy, target string) (net.Conn, *bufio.Reader, string) {
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	return c, br, strings.TrimSpace(status)
}

func TestProxyAllowDenyLog(t *testing.T) {
	up := newUpstream(t)
	var logged []string
	var lmu sync.Mutex
	px, err := StartProxy(ProxyConfig{Hosts: AllowList([]string{"git.example.com", "downloads.claude.ai", "intake.datadoghq.com"}, []string{"github.com"}),
		Dial: up.dial, Logf: func(f string, a ...any) { lmu.Lock(); logged = append(logged, fmt.Sprintf(f, a...)); lmu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	if !strings.HasPrefix(px.Addr(), "127.0.0.1:") {
		t.Fatalf("addr %s", px.Addr())
	}
	for _, h := range []string{"api.anthropic.com", "API.Anthropic.com", "claude.ai", "console.anthropic.com", "platform.claude.com",
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
	if d := up.dialed(); len(d) != 10 || d[0] != "api.anthropic.com:443" || d[1] != "api.anthropic.com:443" {
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
	fmt.Fprint(c, "GET http://api.anthropic.com/ HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n")
	if st, _ := bufio.NewReader(c).ReadString('\n'); !strings.Contains(st, "405") {
		t.Errorf("plain GET: %s", st)
	}
	c.Close()
	if len(up.dialed()) != before {
		t.Fatalf("a refused request reached upstream: %v", up.dialed()[before:])
	}
	refused, opened := px.Refused()
	if opened != 10 || len(refused) != 13 {
		t.Fatalf("refused %v opened %d", refused, opened)
	}
	lmu.Lock()
	defer lmu.Unlock()
	if len(logged) != 13 || !strings.Contains(logged[0], "datadoghq.com") {
		t.Fatalf("log: %v", logged)
	}
}

func TestProxyUpstreamFailureAndClose(t *testing.T) {
	px, err := StartProxy(ProxyConfig{Hosts: []string{"api.anthropic.com"},
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

func TestAllowListNeverHasRefusedHosts(t *testing.T) {
	l := AllowList([]string{"Git.Example.com.", "downloads.claude.ai", "x.datadoghq.eu", "10.0.0.1", "bad host", ""}, nil)
	want := []string{"api.anthropic.com", "claude.ai", "console.anthropic.com", "files.pythonhosted.org", "git.example.com",
		"platform.claude.com", "pypi.org", "registry.npmjs.org"}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("%v", l)
	}
}

func TestSwiftPMHosts(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Package.resolved"), []byte(`{"pins":[
	 {"identity":"a","location":"https://github.com/alice/a.git","state":{}},
	 {"identity":"b","location":"https://gitlab.example.com/bob/b","state":{}},
	 {"identity":"c","location":"git@github.com:alice/c.git"},
	 {"identity":"d","location":"https://downloads.claude.ai/x"},
	 {"identity":"e","location":"file:///tmp/e"}],"version":3}`), 0o600)
	ws := filepath.Join(dir, "App.xcworkspace/xcshareddata/swiftpm")
	os.MkdirAll(ws, 0o700)
	os.WriteFile(filepath.Join(ws, "Package.resolved"), []byte(`{"object":{"pins":[{"package":"z","repositoryURL":"https://swiftpm.example.net/z.git"}]},"version":1}`), 0o600)
	got := SwiftPMHosts(dir)
	if !reflect.DeepEqual(got, []string{"github.com", "gitlab.example.com", "swiftpm.example.net"}) {
		t.Fatalf("%v", got)
	}
	if len(SwiftPMHosts(t.TempDir())) != 0 {
		t.Fatal("hosts from nowhere")
	}
}

func TestSwiftPMHostsReachProxyAllowList(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.proj, "Package.resolved"), []byte(`{"pins":[{"location":"https://swiftpm.example.net/z.git"}]}`), 0o600)
	e.op("sessions", map[string]any{"project": "my-project"})
	e.m.mu.Lock()
	hosts := e.m.projs["my-project"].hosts
	e.m.mu.Unlock()
	found := map[string]bool{}
	for _, h := range hosts {
		found[h] = true
	}
	if !found["swiftpm.example.net"] || !found["git.example.com"] || !found["api.anthropic.com"] {
		t.Fatalf("%v", hosts)
	}
}
