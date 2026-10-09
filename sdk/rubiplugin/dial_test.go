package rubiplugin

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// echo is the "mail server": it greets first, then echoes a line.
func echo(t *testing.T) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.WriteString(c, "* OK hello\r\n")
				l, _ := bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, l)
			}()
		}
	}()
	return ln.Addr().String()
}

func pipe(a, b net.Conn) {
	go func() { _, _ = io.Copy(a, b); a.Close() }()
	_, _ = io.Copy(b, a)
	b.Close()
}

func connectProxy(t *testing.T) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				br := bufio.NewReader(c)
				line, _ := br.ReadString('\n')
				hasUA := false
				for {
					l, _ := br.ReadString('\n')
					hasUA = hasUA || strings.HasPrefix(strings.ToLower(l), "user-agent:")
					if l == "\r\n" || l == "" {
						break
					}
				}
				if !hasUA { // a strict proxy, like some egress proxies: no answer at all
					c.Close()
					return
				}
				target := strings.Fields(line)[1]
				up, err := net.Dial("tcp", target)
				if err != nil {
					_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					c.Close()
					return
				}
				tunnels.Add(1)
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				pipe(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

func socksProxy(t *testing.T) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 512)
				if _, err := io.ReadFull(c, buf[:2]); err != nil || buf[0] != 5 {
					// Not SOCKS (e.g. an HTTP CONNECT probe): answer like a SOCKS server would not.
					_, _ = c.Write([]byte{5, 0xff})
					c.Close()
					return
				}
				_, _ = io.ReadFull(c, buf[:buf[1]])
				_, _ = c.Write([]byte{5, 0})
				_, _ = io.ReadFull(c, buf[:5])
				n := int(buf[4])
				_, _ = io.ReadFull(c, buf[:n+2])
				host := string(buf[:n])
				port := int(buf[n])<<8 | int(buf[n+1])
				up, err := net.Dial("tcp", net.JoinHostPort(host, itoa(port)))
				if err != nil {
					_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					c.Close()
					return
				}
				tunnels.Add(1)
				_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				pipe(c, up)
			}()
		}
	}()
	return ln.Addr().String()
}

func itoa(n int) string { return strconv.Itoa(n) }

var tunnels atomic.Int64

func talk(t *testing.T, addr string) {
	t.Helper()
	c, err := Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	if l, _ := br.ReadString('\n'); l != "* OK hello\r\n" {
		t.Fatalf("greeting %q", l)
	}
	_, _ = io.WriteString(c, "ping\r\n")
	if l, _ := br.ReadString('\n'); l != "ping\r\n" {
		t.Fatalf("echo %q", l)
	}
}

func clearProxyEnv(t *testing.T) {
	for _, k := range EgressProxyEnv {
		t.Setenv(k, "")
	}
}

func TestDialThroughProxies(t *testing.T) {
	target := echo(t)
	clearProxyEnv(t)
	talk(t, target) // no proxy: direct
	via := func(name string) {
		t.Helper()
		before := tunnels.Load()
		talk(t, target)
		if tunnels.Load() != before+1 {
			t.Fatalf("%s: the connection didn't go through the proxy", name)
		}
	}
	t.Setenv("SAND_EGRESS_TUNNEL_PROXY_ADDR", connectProxy(t)) // no scheme: probed, HTTP CONNECT
	via("http probe")
	t.Setenv("SAND_EGRESS_TUNNEL_PROXY_ADDR", socksProxy(t)) // no scheme: probed, SOCKS5
	via("socks probe")
	clearProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks5://"+socksProxy(t))
	via("socks5 scheme")

	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // a dead proxy: falls back to a direct connection
	talk(t, target)
}
