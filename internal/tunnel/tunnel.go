// Package tunnel keeps a Cloudflare quick tunnel open to the local panel API.
//
// Quick tunnels need no account and no configuration, which is what lets Rubi work with zero setup on
// machines that have no inbound address. The URL changes every time the tunnel restarts; Rubi reports
// each new URL to the core, and links always carry the current one. The tunnel only carries the
// end-to-end encrypted panel protocol, never the agent's MCP traffic.
package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	urlRe       = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)
	rateLimitRe = regexp.MustCompile(`(?i)status 429|too many requests`)
	ErrNoBinary = errors.New("cloudflared not found: install it, set RUBI_CLOUDFLARED, or set RUBI_PUBLIC_URL to use another transport (e.g. Tailscale)")
	// ErrRateLimited means Cloudflare refused to create a quick tunnel (HTTP 429). Agent machines often
	// share an egress address, so this can happen through no fault of this instance.
	ErrRateLimited = errors.New("Cloudflare is temporarily refusing new quick tunnels from this machine's network (HTTP 429, too many requests)")
)

// Backoff after failures. A rate limit gets much longer waits: retrying quickly only extends it.
var (
	minBackoff       = 2 * time.Second
	maxBackoff       = time.Minute
	minRateLimitWait = time.Minute
	maxRateLimitWait = 10 * time.Minute
)

// tailLines is how much of cloudflared's output is kept to explain a failure.
const tailLines = 6

// FindCloudflared looks in $RUBI_CLOUDFLARED, then $PATH, then $RUBI_HOME/bin.
func FindCloudflared(home string) (string, error) {
	if p := os.Getenv("RUBI_CLOUDFLARED"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p, nil
	}
	p := filepath.Join(home, "bin", "cloudflared")
	if st, err := os.Stat(p); err == nil && st.Mode()&0o111 != 0 {
		return p, nil
	}
	return "", ErrNoBinary
}

type Manager struct {
	Binary string
	Target string           // e.g. http://127.0.0.1:41234
	OnURL  func(url string) // "" when the tunnel is down
	// OnError explains why there is no tunnel ("" once one is up), so the agent can tell the user.
	OnError func(msg string)
	Logf    func(format string, args ...any)
}

// Run keeps the tunnel up until ctx ends, restarting it with backoff.
func (m *Manager) Run(ctx context.Context) {
	backoff, limited := minBackoff, minRateLimitWait
	for ctx.Err() == nil {
		start := time.Now()
		err := m.runOnce(ctx)
		m.OnURL("")
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 5*time.Minute {
			backoff, limited = minBackoff, minRateLimitWait
		}
		wait := backoff
		if errors.Is(err, ErrRateLimited) {
			wait = limited
			limited = min(limited*2, maxRateLimitWait)
			m.setError(fmt.Sprintf("%v. Rubi retries automatically (next attempt in %s); try again later. "+
				"To avoid this entirely, use Tailscale and set RUBI_PUBLIC_URL.", ErrRateLimited, wait.Round(time.Second)))
		} else {
			backoff = min(backoff*2, maxBackoff)
			m.setError("The panel connection (Cloudflare tunnel) is down; Rubi is restarting it. Try again in a minute.")
		}
		m.Logf("tunnel stopped (%v); restarting in %s", err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (m *Manager) setError(msg string) {
	if m.OnError != nil {
		m.OnError(msg)
	}
}

func (m *Manager) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, m.Binary, "tunnel", "--no-autoupdate", "--url", m.Target)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	var pending string
	var tail []string
	announced := false
	sc := bufio.NewScanner(stderr)
	for sc.Scan() {
		line := sc.Text()
		tail = append(tail, line)
		if len(tail) > tailLines {
			tail = tail[1:]
		}
		if u := urlRe.FindString(line); u != "" && pending == "" {
			pending = u
		}
		// Announce only once the edge has registered a connection and the hostname resolves, so links
		// never point at a dead URL (resolvers cache "no such host" if asked too early).
		if !announced && pending != "" && strings.Contains(line, "Registered tunnel connection") {
			announced = true
			go func(u string) {
				waitForDNS(ctx, u, m.Logf)
				if ctx.Err() == nil {
					m.Logf("tunnel up: %s", u)
					m.setError("")
					m.OnURL(u)
				}
			}(pending)
		}
	}
	return exitError(cmd.Wait(), tail)
}

// exitError explains why cloudflared exited, using the end of its output.
func exitError(err error, tail []string) error {
	if err == nil {
		err = errors.New("exited")
	}
	for _, l := range tail {
		if rateLimitRe.MatchString(l) {
			return fmt.Errorf("%w: %v", ErrRateLimited, err)
		}
	}
	if len(tail) == 0 {
		return err
	}
	return fmt.Errorf("%v; cloudflared said: %s", err, strings.Join(tail, " | "))
}

// waitForDNS polls Cloudflare's DNS-over-HTTPS resolver until the tunnel hostname has an address.
// Asking a public DoH resolver avoids planting a negative entry in the local resolver cache. If DoH is
// unreachable, it falls back to a fixed delay.
func waitForDNS(ctx context.Context, tunnelURL string, logf func(string, ...any)) {
	u, err := url.Parse(tunnelURL)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 5 * time.Second}
	// The record usually appears a few seconds after registration. Asking earlier gets a negative answer
	// that resolvers cache for about a minute, so the first query waits.
	sleep(ctx, 8*time.Second)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		ok, err := resolvesViaDoH(ctx, client, u.Hostname())
		if err != nil {
			logf("DoH check unavailable (%v); waiting 10s instead", err)
			sleep(ctx, 10*time.Second)
			return
		}
		if ok {
			return
		}
		sleep(ctx, 3*time.Second)
	}
}

func resolvesViaDoH(ctx context.Context, c *http.Client, host string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://cloudflare-dns.com/dns-query?type=A&name="+url.QueryEscape(host), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := c.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var r struct {
		Status int               `json:"Status"`
		Answer []json.RawMessage `json:"Answer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return false, err
	}
	return r.Status == 0 && len(r.Answer) > 0, nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
