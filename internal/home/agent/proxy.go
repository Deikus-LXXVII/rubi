package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The allow-list proxy: HTTP CONNECT only, to port 443, to hosts on a list. TLS is not decrypted: the
// proxy sees the host name and nothing else. Everything else is refused and logged.

// BaseHosts are always allowed: the model API and the registries the runner's tools use.
var BaseHosts = []string{"api.anthropic.com", "registry.npmjs.org", "pypi.org", "files.pythonhosted.org"}

// LoginHosts are the candidates for signing in and refreshing the login. TO CONFIRM with a real login
// (the M0 spike's login had expired, so none was observed); they are allowed until then so a login can
// work at all.
var LoginHosts = []string{"claude.ai", "console.anthropic.com", "platform.claude.com"}

// refusedHost reports hosts that are refused even if someone lists them: telemetry and the updater.
func refusedHost(h string) bool {
	return h == "downloads.claude.ai" || h == "datadoghq.com" || strings.HasSuffix(h, ".datadoghq.com") ||
		h == "datadoghq.eu" || strings.HasSuffix(h, ".datadoghq.eu")
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// normalizeHost lowercases and drops a trailing dot; "" if it isn't a plain DNS name (IP literals are
// refused: an allow-list is about names).
func normalizeHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if len(h) > 253 || !hostRE.MatchString(h) || net.ParseIP(h) != nil {
		return ""
	}
	return h
}

// AllowList returns the sorted, de-duplicated hosts of the proxy for a project: the base hosts, the login
// candidates, the configured git hosts and the extra hosts (SwiftPM). Refused hosts never get in.
func AllowList(gitHosts, extra []string) []string {
	set := map[string]bool{}
	for _, list := range [][]string{BaseHosts, LoginHosts, gitHosts, extra} {
		for _, h := range list {
			if n := normalizeHost(h); n != "" && !refusedHost(n) {
				set[n] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

const maxPackageResolved = 4 << 20

// SwiftPMHosts reads the https hosts of the Swift packages a project depends on, from Package.resolved
// (format 1 "repositoryURL", formats 2 and 3 "location"), at the project root and in Xcode's workspace
// location. It is read on the Mac before the VM starts, once per start: the file is in /work, which the
// agent can write, so a file changed during a run does not widen the list.
func SwiftPMHosts(dir string) []string {
	paths := []string{filepath.Join(dir, "Package.resolved")}
	for _, pat := range []string{"*.xcworkspace/xcshareddata/swiftpm/Package.resolved", "*.xcodeproj/project.xcworkspace/xcshareddata/swiftpm/Package.resolved"} {
		m, _ := filepath.Glob(filepath.Join(dir, pat))
		paths = append(paths, m...)
	}
	set := map[string]bool{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(f, maxPackageResolved))
		f.Close()
		for _, h := range packageHosts(b) {
			set[h] = true
		}
	}
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

var pinURLRE = regexp.MustCompile(`"(?:location|repositoryURL)"\s*:\s*"([^"]+)"`)

func packageHosts(b []byte) []string {
	var out []string
	for _, m := range pinURLRE.FindAllSubmatch(b, -1) {
		u, err := url.Parse(string(m[1]))
		if err != nil || u.Scheme != "https" {
			continue // ssh and file URLs don't go through a CONNECT proxy
		}
		if h := normalizeHost(u.Hostname()); h != "" && !refusedHost(h) {
			out = append(out, h)
		}
	}
	return out
}

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	// Listen is the address to listen on ("127.0.0.1:0" when empty). A VM can't reach the Mac's
	// 127.0.0.1: for a real VM set this to the address of the Mac on the VM's host-only network.
	Listen string
	// Hosts is the allow-list (see AllowList).
	Hosts []string
	// Dial connects to the upstream (net.Dialer when nil; a seam for tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logf receives refusals (log.Printf-like); nil discards.
	Logf func(format string, args ...any)
}

// Proxy is a running allow-list proxy.
type Proxy struct {
	cfg   ProxyConfig
	allow map[string]bool
	ln    net.Listener
	slots chan struct{}

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	refused []string
	allowed int
	closed  bool
	wg      sync.WaitGroup
}

const (
	proxyMaxConns   = 128
	proxyMaxHeader  = 16 << 10
	proxyHeaderWait = 10 * time.Second
	proxyDialWait   = 15 * time.Second
	proxyMaxRefused = 100
)

// StartProxy listens and serves in the background.
func StartProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{cfg: cfg, ln: ln, allow: map[string]bool{}, slots: make(chan struct{}, proxyMaxConns), conns: map[net.Conn]struct{}{}}
	for _, h := range cfg.Hosts {
		if n := normalizeHost(h); n != "" && !refusedHost(n) {
			p.allow[n] = true
		}
	}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the address the proxy listens on.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Allowed reports whether a host may be reached.
func (p *Proxy) Allowed(host string) bool {
	h := normalizeHost(host)
	return h != "" && !refusedHost(h) && p.allow[h]
}

// Refused returns the hosts refused so far (at most 100, newest last) and the number of tunnels opened.
func (p *Proxy) Refused() (refused []string, opened int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.refused...), p.allowed
}

// Close stops the proxy and ends every tunnel.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *Proxy) logf(format string, args ...any) {
	if p.cfg.Logf != nil {
		p.cfg.Logf(format, args...)
	}
}

func (p *Proxy) refuse(target, why string) {
	p.mu.Lock()
	if len(p.refused) >= proxyMaxRefused {
		p.refused = p.refused[1:]
	}
	p.refused = append(p.refused, target)
	p.mu.Unlock()
	p.logf("agent proxy: refused %q (%s)", clip(target, 120), why)
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (p *Proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.mu.Lock()
	delete(p.conns, c)
	p.mu.Unlock()
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if !p.track(c) {
			c.Close()
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			p.untrack(c)
			c.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.slots }()
			defer p.untrack(c)
			defer c.Close()
			p.handle(c)
		}()
	}
}

func reply(c net.Conn, code int, text string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", code, text)
}

func (p *Proxy) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(proxyHeaderWait))
	br := bufio.NewReader(io.LimitReader(c, proxyMaxHeader))
	req, err := http.ReadRequest(br)
	if err != nil {
		p.refuse("(unreadable request)", err.Error())
		reply(c, 400, "Bad Request")
		return
	}
	if req.Method != http.MethodConnect {
		p.refuse(req.Method+" "+req.Host, "only CONNECT is served")
		reply(c, 405, "Method Not Allowed")
		return
	}
	host, port, err := net.SplitHostPort(req.Host)
	if err != nil {
		p.refuse(req.Host, "no port")
		reply(c, 400, "Bad Request")
		return
	}
	if port != "443" {
		p.refuse(req.Host, "port")
		reply(c, 403, "Forbidden")
		return
	}
	if !p.Allowed(host) {
		p.refuse(req.Host, "host not on the list")
		reply(c, 403, "Forbidden")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyDialWait)
	defer cancel()
	dial := p.cfg.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	up, err := dial(ctx, "tcp", net.JoinHostPort(normalizeHost(host), "443"))
	if err != nil {
		reply(c, 502, "Bad Gateway")
		return
	}
	if !p.track(up) {
		up.Close()
		return
	}
	defer p.untrack(up)
	defer up.Close()
	p.mu.Lock()
	p.allowed++
	p.mu.Unlock()
	_ = c.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// Whatever the client sent after the headers goes on; then both directions until one ends.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := up.Write(b); err != nil {
			return
		}
	}
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(up, c); closeWrite(up); errc <- err }()
	go func() { _, err := io.Copy(c, up); closeWrite(c); errc <- err }()
	<-errc
	<-errc
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		c.Close()
	}
}
