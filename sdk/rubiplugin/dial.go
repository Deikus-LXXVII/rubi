package rubiplugin

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Dial opens a TCP connection to addr ("host:port") for a plugin, through the machine's egress proxy when
// it has one. Some agent machines let only web traffic out directly (port 443) and send everything else,
// such as IMAP on 993, through a local proxy; Rubi passes its address to plugins (EgressProxyEnv).
// Whatever is sent stays end-to-end encrypted when the plugin speaks TLS over the connection: the proxy
// only carries bytes.
//
// The proxy is tried first; if it can't be used, Dial connects directly. Both HTTP CONNECT and SOCKS5
// proxies work; an address without a scheme is probed for either.
func Dial(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 20 * time.Second}
	proxy := egressProxy()
	if proxy == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	trace("egress proxy %s (%s)", proxy.Host, proxy.Scheme)
	conn, perr := dialVia(ctx, d, proxy, addr)
	if perr == nil {
		return conn, nil
	}
	trace("through the proxy: %v; trying directly", perr)
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// The proxy is the way out here: its error is the one that explains.
		return nil, fmt.Errorf("through the egress proxy %s: %v (directly: %v)", proxy.Host, perr, err)
	}
	return conn, nil
}

// Trace, when set, is told each step of Dial (rubi net-check uses it to show what happens).
var Trace func(format string, args ...any)

func trace(format string, args ...any) {
	if Trace != nil {
		Trace(format, args...)
	}
}

// EgressProxyEnv lists the environment variables that name an egress proxy, in order of preference. Rubi
// passes these (and only these) on to plugins.
var EgressProxyEnv = []string{"RUBI_EGRESS_PROXY", "SAND_EGRESS_TUNNEL_PROXY_ADDR", "ALL_PROXY", "all_proxy",
	"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

func egressProxy() *url.URL {
	for _, k := range EgressProxyEnv {
		if strings.EqualFold(k, "NO_PROXY") {
			continue
		}
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "auto://" + v
		}
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			return u
		}
	}
	return nil
}

func dialVia(ctx context.Context, d *net.Dialer, proxy *url.URL, addr string) (net.Conn, error) {
	switch proxy.Scheme {
	case "socks5", "socks5h":
		return dialSOCKS(ctx, d, proxy, addr)
	case "http", "https":
		return dialCONNECT(ctx, d, proxy, addr)
	}
	conn, err := dialCONNECT(ctx, d, proxy, addr)
	if errors.Is(err, errNotHTTP) {
		return dialSOCKS(ctx, d, proxy, addr)
	}
	return conn, err
}

var errNotHTTP = errors.New("the proxy doesn't speak HTTP")

func dialCONNECT(ctx context.Context, d *net.Dialer, proxy *url.URL, addr string) (net.Conn, error) {
	conn, err := d.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	// The headers curl sends: strict proxies close connections that lack them.
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\nUser-Agent: Rubi\r\nProxy-Connection: Keep-Alive\r\n"
	if u := proxy.User; u != nil {
		p, _ := u.Password()
		req += "Proxy-Authorization: Basic " + basic(u.Username(), p) + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		conn.Close()
		return nil, err
	}
	trace("CONNECT %s sent", addr)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	trace("proxy answered %q (%v)", strings.TrimSpace(status), err)
	if status == "" { // no answer at all: not a protocol question, the proxy turned the request down
		conn.Close()
		return nil, fmt.Errorf("the proxy closed the connection without answering CONNECT (%v)", err)
	}
	if !strings.HasPrefix(status, "HTTP/") { // a SOCKS proxy answers a few bytes and hangs up
		conn.Close()
		return nil, errNotHTTP
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	if f := strings.Fields(status); len(f) < 2 || f[1] != "200" {
		conn.Close()
		return nil, fmt.Errorf("the proxy refused the connection: %s", strings.TrimSpace(status))
	}
	for { // the rest of the response headers
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 { // the server spoke first: keep what was already read
		return &bufferedConn{Conn: conn, r: br}, nil
	}
	return conn, nil
}

func dialSOCKS(ctx context.Context, d *net.Dialer, proxy *url.URL, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || len(host) > 255 {
		return nil, errors.New("bad address")
	}
	conn, err := d.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fail := func(err error) (net.Conn, error) { conn.Close(); return nil, err }
	user, pass := "", ""
	if proxy.User != nil {
		user = proxy.User.Username()
		pass, _ = proxy.User.Password()
	}
	methods := []byte{5, 1, 0}
	if user != "" {
		methods = []byte{5, 2, 0, 2}
	}
	if _, err := conn.Write(methods); err != nil {
		return fail(err)
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[0] != 5 {
		return fail(errors.New("the proxy doesn't speak SOCKS5"))
	}
	switch reply[1] {
	case 0:
	case 2:
		if len(user) > 255 || len(pass) > 255 {
			return fail(errors.New("proxy credentials too long"))
		}
		msg := append(append(append([]byte{1, byte(len(user))}, user...), byte(len(pass))), pass...)
		if _, err := conn.Write(msg); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil || reply[1] != 0 {
			return fail(errors.New("the proxy rejected its credentials"))
		}
	default:
		return fail(errors.New("the proxy accepts no usable login method"))
	}
	// The proxy resolves the name: this machine's own DNS may only know placeholder addresses.
	req := append([]byte{5, 1, 0, 3, byte(len(host))}, host...)
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fail(err)
	}
	if head[1] != 0 {
		return fail(fmt.Errorf("the proxy couldn't connect (SOCKS error %d)", head[1]))
	}
	var skip int
	switch head[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return fail(err)
		}
		skip = int(n[0])
	default:
		return fail(errors.New("bad SOCKS reply"))
	}
	if _, err := io.CopyN(io.Discard, conn, int64(skip+2)); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func basic(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
