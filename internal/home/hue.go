package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Philips Hue, through the bridge's local API (CLIP v2). The helper finds bridges on the home network,
// pairs with one when the user presses its link button, and then passes Rubi's requests to it. The
// application key stays in Rubi's vault and comes with each request. The bridge's certificate is
// recorded when pairing (its name is the bridge id) and must match on every later connection, so a
// device that takes over the bridge's address on the network gets nothing.

type Hue struct {
	Pins   func() map[string]string
	SetPin func(bridge, pin string) error
	// Discovery is the cloud fallback for finding bridges (a variable for tests).
	Discovery string
	// Insecure is for tests: addresses may carry a port and certificates aren't matched to the bridge id.
	Insecure bool
	// configHost, in tests, is where the bridge's plain-HTTP config is served, and find replaces the
	// network search.
	configHost string
	find       func(ctx context.Context) []string

	mu   sync.Mutex
	last map[string]time.Time // per bridge, to space requests
}

type Bridge struct {
	ID string `json:"id"`
	IP string `json:"ip"`
}

// HueRequest is one call to a bridge's resource API.
type HueRequest struct {
	Bridge string          `json:"bridge"`
	IP     string          `json:"ip"`
	Key    string          `json:"key"`
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

var (
	resourcePath = regexp.MustCompile(`^/clip/v2/resource(/[a-z_]+(/[0-9a-f-]{36})?)?$`)
	bridgeIDRe   = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

func normBridge(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// Discover finds bridges: mDNS on the local network, then Signify's discovery service.
func (h *Hue) Discover(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	ips := map[string]bool{}
	if h.find != nil {
		for _, ip := range h.find(ctx) {
			ips[ip] = true
		}
	} else {
		for _, ip := range mdnsHue(ctx, 2*time.Second) {
			ips[ip] = true
		}
		if len(ips) == 0 {
			for _, ip := range h.cloudDiscover(ctx) {
				ips[ip] = true
			}
		}
	}
	bridges := []Bridge{}
	for ip := range ips {
		if id, err := h.bridgeID(ctx, ip); err == nil {
			bridges = append(bridges, Bridge{ID: id, IP: ip})
		}
	}
	return map[string]any{"bridges": bridges}, nil
}

func (h *Hue) cloudDiscover(ctx context.Context) []string {
	u := h.Discovery
	if u == "" {
		u = "https://discovery.meethue.com/"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var list []struct {
		IP string `json:"internalipaddress"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&list) != nil {
		return nil
	}
	var out []string
	for _, b := range list {
		if net.ParseIP(b.IP) != nil {
			out = append(out, b.IP)
		}
	}
	return out
}

// bridgeID asks a device for its bridge id (no key needed); it also proves the device is a bridge.
func (h *Hue) bridgeID(ctx context.Context, ip string) (string, error) {
	if !h.validAddr(ip) {
		return "", errors.New("not an IP address")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	host := ip
	if h.configHost != "" {
		host = h.configHost
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+host+"/api/config", nil)
	resp, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var cfg struct {
		BridgeID string `json:"bridgeid"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&cfg); err != nil {
		return "", err
	}
	id := normBridge(cfg.BridgeID)
	if !bridgeIDRe.MatchString(id) {
		return "", errors.New("not a Hue Bridge")
	}
	return id, nil
}

// Pair asks the bridge for an application key. The user must have pressed the link button.
func (h *Hue) Pair(ctx context.Context, ip string) (any, error) {
	id, err := h.bridgeID(ctx, ip)
	if err != nil {
		// No detail: what a device at some address answered must not turn Rubi Home into a network scanner.
		return nil, fmt.Errorf("no Hue Bridge answered at %s", ip)
	}
	var pin string
	client := h.client(func(cert *x509.Certificate) error {
		if !h.Insecure && normBridge(cert.Subject.CommonName) != id {
			return errors.New("the bridge's certificate doesn't match its id")
		}
		sum := sha256.Sum256(cert.Raw)
		pin = hex.EncodeToString(sum[:])
		return nil
	})
	body := []byte(`{"devicetype":"rubi#home","generateclientkey":true}`)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url(ip, "/api"), bytes.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []struct {
		Success *struct {
			Username string `json:"username"`
		} `json:"success"`
		Error *struct {
			Type        int    `json:"type"`
			Description string `json:"description"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || len(out) == 0 {
		return nil, errors.New("unexpected answer from the bridge")
	}
	if e := out[0].Error; e != nil {
		if e.Type == 101 {
			return nil, errors.New("press the round link button on the Hue Bridge, then try again within 30 seconds")
		}
		return nil, errors.New("the bridge said: " + e.Description)
	}
	if out[0].Success == nil || out[0].Success.Username == "" {
		return nil, errors.New("the bridge didn't give a key")
	}
	if old := h.Pins()[id]; old != "" && old != pin {
		return nil, fmt.Errorf("this bridge's certificate differs from the one recorded when it was first paired; if you "+
			"replaced or reset the bridge, run `rubi-home hue-forget %s` on the home computer and set Hue up again", id)
	}
	if err := h.SetPin(id, pin); err != nil {
		return nil, err
	}
	return map[string]any{"bridge": id, "ip": ip, "key": out[0].Success.Username}, nil
}

func (h *Hue) url(ip, path string) string {
	return (&url.URL{Scheme: "https", Host: ip, Path: path}).String()
}

// validAddr accepts only an IP address on the home network (tests may add a port and use loopback): a
// bridge is never on the internet, on this computer itself, or a broadcast address.
func (h *Hue) validAddr(ip string) bool {
	if h.Insecure {
		host, _, err := net.SplitHostPort(ip)
		return err == nil && net.ParseIP(host) != nil
	}
	a := net.ParseIP(ip)
	return a != nil && (a.IsPrivate() || a.IsLinkLocalUnicast()) && !a.IsLoopback()
}

// noRedirect keeps requests at the address they were sent to: a redirect could send them anywhere on
// the home network, or downgrade them to plain http and skip the certificate check.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// client returns an HTTP client that checks the bridge certificate with check (instead of a CA).
func (h *Hue) client(check func(*x509.Certificate) error) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: noRedirect, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("the bridge sent no certificate")
				}
				return check(cs.PeerCertificates[0])
			}},
		DisableKeepAlives: true,
	}}
}

// Do passes one request to a paired bridge. If the bridge can't be reached at its last address, it is
// looked for again by id (its address may have changed).
func (h *Hue) Do(ctx context.Context, in HueRequest) (any, error) {
	id := normBridge(in.Bridge)
	pin := h.Pins()[id]
	if pin == "" {
		return nil, errors.New("this Hue Bridge wasn't paired through this Rubi Home; set Hue up again")
	}
	method := strings.ToUpper(in.Method)
	if method != http.MethodGet && method != http.MethodPut {
		return nil, errors.New("only GET and PUT are allowed")
	}
	if !resourcePath.MatchString(in.Path) {
		return nil, errors.New("only the bridge's resource API (/clip/v2/resource/…) is allowed")
	}
	if !h.validAddr(in.IP) {
		return nil, errors.New("the bridge address must be an IP address")
	}
	if len(in.Body) > 16<<10 {
		return nil, errors.New("request too large")
	}
	h.space(id)
	client := h.client(func(cert *x509.Certificate) error {
		sum := sha256.Sum256(cert.Raw)
		if hex.EncodeToString(sum[:]) != pin {
			return errors.New("the device at this address isn't your Hue Bridge (its certificate changed)")
		}
		return nil
	})
	ip := in.IP
	status, body, err := h.send(ctx, client, ip, method, in.Path, in.Key, in.Body)
	var opErr *net.OpError
	if err != nil && errors.As(err, &opErr) && opErr.Op == "dial" { // gone from that address, not a wrong device
		if found, _ := h.Discover(ctx); found != nil {
			for _, b := range found.(map[string]any)["bridges"].([]Bridge) {
				if b.ID == id && b.IP != ip {
					ip = b.IP
					status, body, err = h.send(ctx, client, ip, method, in.Path, in.Key, in.Body)
				}
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't reach the Hue Bridge: %w", err)
	}
	return map[string]any{"status": status, "body": json.RawMessage(body), "ip": ip}, nil
}

func (h *Hue) send(ctx context.Context, client *http.Client, ip, method, path, key string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.url(ip, path), rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("hue-application-key", key)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if !json.Valid(b) {
		b, _ = json.Marshal(string(b))
	}
	return resp.StatusCode, b, err
}

// space keeps at least 100 ms between requests to one bridge (it rejects bursts).
func (h *Hue) space(bridge string) {
	h.mu.Lock()
	if h.last == nil {
		h.last = map[string]time.Time{}
	}
	wait := time.Until(h.last[bridge].Add(100 * time.Millisecond))
	if wait < 0 {
		wait = 0
	}
	h.last[bridge] = time.Now().Add(wait)
	h.mu.Unlock()
	time.Sleep(wait)
}

// mdnsHue asks the local network for Hue bridges (_hue._tcp) and returns the addresses that answered.
func mdnsHue(ctx context.Context, wait time.Duration) []string {
	name, _ := dnsmessage.NewName("_hue._tcp.local.")
	msg := dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypePTR,
		Class: dnsmessage.ClassINET | 1<<15}}} // ask for unicast replies
	packet, err := msg.Pack()
	if err != nil {
		return nil
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil
	}
	defer conn.Close()
	if _, err := conn.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
		return nil
	}
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	seen := map[string]bool{}
	var out []string
	buf := make([]byte, 9000)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return out
		}
		var p dnsmessage.Parser
		if _, err := p.Start(buf[:n]); err != nil {
			continue
		}
		_ = p.SkipAllQuestions()
		hue := false
		for {
			a, err := p.AnswerHeader()
			if err != nil {
				break
			}
			hue = hue || strings.HasPrefix(strings.ToLower(a.Name.String()), "_hue._tcp.")
			_ = p.SkipAnswer()
		}
		if ip := from.IP.String(); hue && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
}
