package agent

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The allow-list proxy: HTTP CONNECT only, to port 443, to hosts on a list. TLS is not decrypted: the
// proxy sees the host name and nothing else. Everything else is refused and logged.

// BaseHosts are always allowed: the registries the runner's tools use. The model API is not among them: the
// runner reaches it only through the auth gateway (gateway.go), which is the only holder of the Claude token.
var BaseHosts = []string{"registry.npmjs.org", "pypi.org", "files.pythonhosted.org"}

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

// ForgeHosts are the well-known code hosts allowed by default for package managers (SwiftPM, go, cargo
// pull from them); config agent_no_default_forges drops them. Nothing is derived from the project's files.
var ForgeHosts = []string{"github.com", "gitlab.com", "bitbucket.org"}

// AllowList returns the sorted, de-duplicated hosts of the proxy for a project: the base hosts, the login
// candidates, the configured git hosts and the extra hosts (the owner's agent_allowed_hosts, the hosts the
// owner approved for this project, the forges). Refused hosts never get in.
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

// ProxyConfig configures a Proxy.
type ProxyConfig struct {
	// Listen is the address to listen on ("127.0.0.1:0" when empty). A wildcard address (empty host,
	// 0.0.0.0, ::) is refused: the proxy would be open to the whole network. (A VM without a network doesn't use
	// this: it is served through a Listener, see pipe.go.)
	Listen string
	// Listener, when set, replaces Listen: the proxy serves the connections it yields and nothing listens on
	// the machine (a VM without a network reaches the proxy through a netpipe stream).
	Listener net.Listener
	// AuthToken is required from every client as the password of Proxy-Authorization: Basic (the user name
	// is ignored); the VM gets it in HTTPS_PROXY. Without it any process that can reach the port would
	// use the Mac's network access.
	AuthToken string
	// Hosts is the allow-list (see AllowList).
	Hosts []string
	// Exempt are hosts that may resolve to non-public addresses (the owner's own git servers).
	Exempt []string
	// Ask, when set, is asked about a valid host name that is not on the list (net.ask); true allows it
	// from now on (the proxy adds it). It is never asked for refused hosts, other ports or non-names.
	Ask func(ctx context.Context, host string) bool
	// Resolve looks a name up (the system resolver when nil) and Dial connects to the resolved address
	// (a guarded net.Dialer when nil); both are seams for tests.
	Resolve func(ctx context.Context, host string) ([]net.IP, error)
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	// IdleTimeout ends a tunnel with no traffic for this long (10 minutes when zero).
	IdleTimeout time.Duration
	// Logf receives refusals (log.Printf-like); nil discards.
	Logf func(format string, args ...any)
}

// Proxy is a running allow-list proxy.
type Proxy struct {
	cfg    ProxyConfig
	exempt map[string]bool
	ln     net.Listener
	slots  chan struct{}

	mu      sync.Mutex
	allow   map[string]bool
	conns   map[net.Conn]string // open upstream tunnels -> their host
	refused []string
	allowed int
	closed  bool
	wg      sync.WaitGroup
}

const (
	proxyMaxConns    = 128
	proxyMaxHeader   = 16 << 10
	proxyHeaderWait  = 10 * time.Second
	proxyDialWait    = 15 * time.Second
	proxyAskWait     = 130 * time.Second
	proxyMaxRefused  = 100
	proxyIdleDefault = 10 * time.Minute
)

// StartProxy listens and serves in the background.
func StartProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.Listener == nil {
		if cfg.Listen == "" {
			cfg.Listen = "127.0.0.1:0"
		}
		if err := validateListen(cfg.Listen, "proxy"); err != nil {
			return nil, err
		}
	}
	if len(cfg.AuthToken) < 16 {
		return nil, errors.New("the proxy needs an authentication token")
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = proxyIdleDefault
	}
	if cfg.Resolve == nil {
		cfg.Resolve = defaultResolve
	}
	ln := cfg.Listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			return nil, err
		}
	}
	p := &Proxy{cfg: cfg, ln: ln, allow: map[string]bool{}, exempt: map[string]bool{}, slots: make(chan struct{}, proxyMaxConns),
		conns: map[net.Conn]string{}}
	for _, h := range cfg.Hosts {
		p.AddHost(h)
	}
	for _, h := range cfg.Exempt {
		if n := normalizeHost(h); n != "" {
			p.exempt[n] = true
		}
	}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// AddHost allows a host from now on (refused hosts and non-names are ignored).
func (p *Proxy) AddHost(h string) {
	if n := normalizeHost(h); n != "" && !refusedHost(n) {
		p.mu.Lock()
		p.allow[n] = true
		p.mu.Unlock()
	}
}

// SetHosts replaces the allow-list (as AllowList builds it): a host that is no longer on it is refused from now on
// and its open tunnels are closed.
func (p *Proxy) SetHosts(hosts []string) {
	allow := map[string]bool{}
	for _, h := range hosts {
		if n := normalizeHost(h); n != "" && !refusedHost(n) {
			allow[n] = true
		}
	}
	p.mu.Lock()
	p.allow = allow
	var drop []net.Conn
	for c, h := range p.conns {
		if h != "" && !allow[h] {
			drop = append(drop, c)
		}
	}
	p.mu.Unlock()
	for _, c := range drop {
		c.Close()
	}
}

// Addr is the address the proxy listens on.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Allowed reports whether a host may be reached.
func (p *Proxy) Allowed(host string) bool {
	h := normalizeHost(host)
	p.mu.Lock()
	defer p.mu.Unlock()
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
	if p == nil {
		return nil
	}
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
	p.refused = append(p.refused, clip(target, 120))
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

func (p *Proxy) track(c net.Conn, host string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = host
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
		if !p.track(c, "") { // a client connection: no host
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

func reply(c net.Conn, code int, text string, extra ...string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\n%sConnection: close\r\nContent-Length: 0\r\n\r\n", code, text, strings.Join(extra, ""))
}

// authorized checks Proxy-Authorization: Basic with the per-start token as the password.
func (p *Proxy) authorized(h http.Header) bool {
	v := h.Get("Proxy-Authorization")
	if len(v) < 7 || !strings.EqualFold(v[:6], "Basic ") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v[6:]))
	if err != nil {
		return false
	}
	_, pass, ok := strings.Cut(string(raw), ":")
	return ok && subtle.ConstantTimeCompare([]byte(pass), []byte(p.cfg.AuthToken)) == 1
}

// idleConn pushes the deadline forward on every read and write, so an idle tunnel ends.
type idleConn struct {
	net.Conn
	d time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetDeadline(time.Now().Add(c.d))
	return c.Conn.Write(b)
}

// upstream resolves host and connects to the first address that works. Unless the host is exempt, any
// address that isn't public stops it (all of them are checked: a name with one private address is refused).
func (p *Proxy) upstream(ctx context.Context, host string) (net.Conn, error) {
	exempt := p.exempt[host]
	ips, err := p.cfg.Resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("no address")
	}
	if !exempt {
		for _, ip := range ips {
			if blockedIP(ip) {
				p.refuse(host, "resolves to "+ip.String())
				return nil, errBlockedAddr
			}
		}
	}
	dial := p.cfg.Dial
	if dial == nil {
		dial = defaultDial(!exempt)
	}
	var last error
	for _, ip := range ips {
		c, err := dial(ctx, "tcp", net.JoinHostPort(ip.String(), "443"))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

func (p *Proxy) handle(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(proxyHeaderWait))
	br := bufio.NewReader(io.LimitReader(c, proxyMaxHeader))
	req, err := http.ReadRequest(br)
	if err != nil {
		reply(c, 400, "Bad Request")
		return
	}
	if !p.authorized(req.Header) {
		reply(c, 407, "Proxy Authentication Required", "Proxy-Authenticate: Basic realm=\"rubi\"\r\n")
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
	host = normalizeHost(host)
	if host == "" || refusedHost(host) {
		p.refuse(req.Host, "host not allowed")
		reply(c, 403, "Forbidden")
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if !p.Allowed(host) {
		ok := false
		if p.cfg.Ask != nil {
			actx, cancel := context.WithTimeout(context.Background(), proxyAskWait)
			ok = p.cfg.Ask(actx, host)
			cancel()
		}
		if !ok {
			p.refuse(req.Host, "host not on the list")
			reply(c, 403, "Forbidden")
			return
		}
		p.AddHost(host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyDialWait)
	defer cancel()
	up, err := p.upstream(ctx, host)
	if err != nil {
		if errors.Is(err, errBlockedAddr) {
			reply(c, 403, "Forbidden")
		} else {
			reply(c, 502, "Bad Gateway")
		}
		return
	}
	if !p.track(up, host) {
		up.Close()
		return
	}
	defer p.untrack(up)
	defer up.Close()
	p.mu.Lock()
	p.allowed++
	p.mu.Unlock()
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
	ci, ui := idleConn{c, p.cfg.IdleTimeout}, idleConn{up, p.cfg.IdleTimeout}
	errc := make(chan error, 2)
	go func() { _, err := io.Copy(ui, ci); closeWrite(up); errc <- err }()
	go func() { _, err := io.Copy(ci, ui); closeWrite(c); errc <- err }()
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

// validateListen refuses an address that isn't one address: no host, or a wildcard. A listener for a VM's
// traffic must be bound to the Mac's address on the VM network, never to every interface.
func validateListen(listen, what string) error {
	lh, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("%s listen address: %w", what, err)
	}
	// An address literal (or localhost), and not a wildcard: "0" or an empty host would listen on every interface.
	ip := net.ParseIP(strings.Trim(lh, "[]"))
	if lh == "" || (ip != nil && ip.IsUnspecified()) || (ip == nil && lh != "localhost") {
		return fmt.Errorf("the %s must listen on one address (the Mac's address on the VM network), not on a wildcard or a name", what)
	}
	return nil
}
