package agent

// The auth gateway: the only place the Claude token is used. A project VM never holds it. Its runner is told
// ANTHROPIC_BASE_URL=http://<gateway>:<port> and a random per-start secret as CLAUDE_CODE_OAUTH_TOKEN (which makes
// claude send "Authorization: Bearer <secret>" with the OAuth beta). The gateway, a plain HTTP reverse proxy on the
// Mac's address on the VM network, checks that secret, throws away every client credential, puts the real token
// from <helper dir>/agent-auth/token/oauth-token on the request and sends it to https://api.anthropic.com, and
// only there. It lives as long as the runner it was started for: the secret is useless afterwards and anywhere else.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// AnthropicAPI is the only upstream the gateway talks to.
	AnthropicAPI = "https://api.anthropic.com"
	// gatewayMaxBody bounds a request (a long conversation with images is a few MiB).
	gatewayMaxBody = 64 << 20
	// gatewayStall ends a response that sends nothing for this long (a stream sends pings every few seconds).
	gatewayStall = 5 * time.Minute
	oauthBeta    = "oauth-2025-04-20"
)

// Vars, so that tests can shorten them.
var (
	// gatewayMaxInFlight is how many requests are served at once; more get 429 (a runner makes a few at a time).
	gatewayMaxInFlight = 64
	// gatewayReadBudget is how long a request, body included, may take to arrive.
	gatewayReadBudget = 5 * time.Minute
	// gatewayWriteChunk is how long one write to the guest may take: a guest that stops reading is cut off.
	gatewayWriteChunk = 60 * time.Second
	// gatewayLogEvery limits the log of one kind of refusal.
	gatewayLogEvery = 10 * time.Second
)

// gatewayPaths are the only requests that are forwarded (the spike in docs/design/home-setup-wizard.md found
// claude sends nothing else to the base URL with the image's environment).
var gatewayPaths = map[string]string{"/v1/messages": http.MethodPost, "/v1/messages/count_tokens": http.MethodPost}

// forwardedRequest are the headers of a client request that are passed on; credentials, cookies and proxy headers
// are not among them.
func forwardedRequest(name string) bool {
	switch name {
	case "accept", "content-type", "anthropic-version", "anthropic-beta", "anthropic-dangerous-direct-browser-access",
		"user-agent", "x-app", "x-claude-code-session-id":
		return true
	}
	return strings.HasPrefix(name, "x-stainless-")
}

// forwardedResponse are the headers of the upstream answer that are passed back.
func forwardedResponse(name string) bool {
	switch name {
	case "content-type", "cache-control", "retry-after", "request-id", "x-request-id", "x-should-retry", "retry-after-ms":
		return true
	}
	return strings.HasPrefix(name, "anthropic-") || strings.HasPrefix(name, "x-ratelimit-")
}

// GatewayConfig configures a Gateway.
type GatewayConfig struct {
	// Listen is where to listen (the Mac's address on the VM network, port 0); loopback is for tests.
	Listen string
	// Listener, when set, replaces Listen (see ProxyConfig.Listener).
	Listener net.Listener
	// Secret is what the runner must present as "Authorization: Bearer <Secret>".
	Secret string
	// TokenFile is the file with the real token. It is read at each request (a sign-in or sign-out applies at once).
	TokenFile string
	// AllowWebSearch lets requests carry the server-side web search tool (default: refused). It is set for the
	// start of a session whose role grants WebSearch; no other server-side tool is ever let through.
	AllowWebSearch bool
	// upstream and client replace https://api.anthropic.com for tests. They are unexported: only code in this
	// package can set them (its tests, or the build-tagged hook in gateway_testhook.go), never a configuration.
	upstream string
	client   *http.Client
	// Logf receives refusals and failures, never a header, a body or a token (nil discards).
	Logf func(format string, args ...any)
}

// Gateway is a running auth gateway.
type Gateway struct {
	cfg      GatewayConfig
	upstream *url.URL
	client   *http.Client
	srv      *http.Server
	ln       net.Listener

	sem chan struct{}

	mu         sync.Mutex
	refusals   map[string]int
	lastLog    map[string]time.Time
	suppressed map[string]int
	served     int
}

// StartGateway starts the gateway.
func StartGateway(cfg GatewayConfig) (*Gateway, error) {
	if len(cfg.Secret) < 32 {
		return nil, errors.New("the gateway needs a long secret")
	}
	if cfg.upstream == "" {
		cfg.upstream = AnthropicAPI
	}
	up, err := url.Parse(cfg.upstream)
	if err != nil || up.Host == "" || up.User != nil {
		return nil, errors.New("bad upstream")
	}
	// The lock on the Anthropic host holds unless the test-only seam (cfg.client) is used.
	if cfg.client == nil && (up.Scheme != "https" || up.Host != "api.anthropic.com") {
		return nil, errors.New("the gateway only talks to https://api.anthropic.com")
	}
	ln := cfg.Listener
	if ln == nil {
		if cfg.Listen == "" {
			cfg.Listen = "127.0.0.1:0"
		}
		if err := validateListen(cfg.Listen, "gateway"); err != nil {
			return nil, err
		}
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			return nil, err
		}
	}
	client := cfg.client
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil // the upstream is reached directly, with the Mac's own network
		tr.ResponseHeaderTimeout = 2 * time.Minute
		client = &http.Client{Transport: tr}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } // the token goes nowhere else
	g := &Gateway{cfg: cfg, upstream: up, client: &c, ln: ln, refusals: map[string]int{}, lastLog: map[string]time.Time{}, suppressed: map[string]int{},
		sem: make(chan struct{}, gatewayMaxInFlight)}
	// ReadTimeout is the budget for a request including its body; there is no WriteTimeout, a response is
	// streamed and each chunk gets its own write deadline (handle).
	g.srv = &http.Server{Handler: http.HandlerFunc(g.handle), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: gatewayReadBudget,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = g.srv.Serve(ln) }()
	return g, nil
}

// Addr is the address the gateway listens on.
func (g *Gateway) Addr() string { return g.ln.Addr().String() }

// Close stops the gateway and ends the requests in flight. It is safe on a nil Gateway.
func (g *Gateway) Close() {
	if g == nil {
		return
	}
	_ = g.srv.Close()
}

// Refusals counts what the gateway refused, by reason (for tests and the status).
func (g *Gateway) Refusals() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string]int{}
	for k, v := range g.refusals {
		out[k] = v
	}
	return out
}

func (g *Gateway) logf(format string, args ...any) {
	if g.cfg.Logf != nil {
		g.cfg.Logf(format, args...)
	}
}

func (g *Gateway) refuse(w http.ResponseWriter, status int, reason, errType, msg string) {
	g.mu.Lock()
	g.refusals[reason]++
	// A guest can make a refusal as often as it likes: the log gets each reason once per interval, with a count.
	now := time.Now()
	var logIt bool
	var n int
	if now.Sub(g.lastLog[reason]) >= gatewayLogEvery {
		g.lastLog[reason], n, g.suppressed[reason] = now, g.suppressed[reason], 0
		logIt = true
	} else {
		g.suppressed[reason]++
	}
	g.mu.Unlock()
	if logIt && n > 0 {
		g.logf("agent gateway: refused (%s), and %d more since the last line", reason, n)
	} else if logIt {
		g.logf("agent gateway: refused (%s)", reason)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errType, "message": msg}})
	_, _ = w.Write(b)
}

// readToken reads the real token: a plain file of at most 4096 bytes that starts with sk-ant-. The file is
// opened without following a symlink and checked through the open descriptor, so what is checked is what is read.
func readToken(file string) (string, bool) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // NONBLOCK: a FIFO in its place doesn't hang the open
	if err != nil {
		return "", false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 || st.Size() > 4096 {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", false
	}
	t := strings.TrimSpace(string(b))
	if !strings.HasPrefix(t, "sk-ant-") || strings.ContainsAny(t, " \t\r\n\x00") {
		return "", false
	}
	return t, true
}

func (g *Gateway) handle(w http.ResponseWriter, r *http.Request) {
	// 0. how many: a bound on what the guest can hold open.
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	default:
		w.Header().Set("Retry-After", "5")
		g.refuse(w, http.StatusTooManyRequests, "too many requests", "rate_limit_error", "Rubi: the agent gateway is busy; try again shortly.")
		return
	}
	// 1. who: only the runner this gateway was started for.
	auth := r.Header.Get("Authorization")
	secret, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(secret), []byte(g.cfg.Secret)) != 1 {
		g.refuse(w, http.StatusUnauthorized, "bad secret", "authentication_error", "Rubi: this request isn't from the agent this gateway belongs to.")
		return
	}
	// 2. what: only the model endpoints claude needs.
	if r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens" {
		g.refuse(w, http.StatusNotFound, "path", "not_found_error", "Rubi: this path isn't served by the agent gateway.")
		return
	}
	if gatewayPaths[r.URL.Path] != r.Method || (r.URL.RawQuery != "" && r.URL.RawQuery != "beta=true") {
		g.refuse(w, http.StatusMethodNotAllowed, "method", "invalid_request_error", "Rubi: this request isn't served by the agent gateway.")
		return
	}
	// 3. signed in?
	token, ok := readToken(g.cfg.TokenFile)
	if !ok {
		g.refuse(w, http.StatusUnauthorized, "not signed in", "authentication_error",
			"Rubi: Rubi Home isn't signed in to Claude. Sign in on the agent setup page, then try again.")
		return
	}
	// 4. the body: no server-side tool, no MCP connector (they run on Anthropic's servers, outside the allow-list).
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, gatewayMaxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			g.refuse(w, http.StatusRequestEntityTooLarge, "body size", "request_too_large", "Rubi: the request is too large for the agent gateway.")
		} else {
			g.refuse(w, http.StatusBadRequest, "request", "invalid_request_error", "Rubi: bad request.")
		}
		return
	}
	if why := checkBody(body, g.cfg.AllowWebSearch); why != "" {
		g.refuse(w, http.StatusForbidden, "server-side feature", "permission_error",
			"Rubi: the agent gateway doesn't pass this request on: "+why+". Server-side tools and the MCP connector run outside Rubi's network allow-list.")
		return
	}
	// 5. forward, with only the headers on the list and the real credential.
	up := *g.upstream
	up.Path, up.RawQuery = r.URL.Path, r.URL.RawQuery
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, r.Method, up.String(), bytes.NewReader(body))
	if err != nil {
		g.refuse(w, http.StatusBadRequest, "request", "invalid_request_error", "Rubi: bad request.")
		return
	}
	for name, vals := range r.Header {
		if forwardedRequest(strings.ToLower(name)) {
			out.Header[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
		}
	}
	out.ContentLength = int64(len(body))
	out.Header.Set("Authorization", "Bearer "+token)
	// Only the betas on the list are passed on (see betaPrefixes), plus the OAuth beta.
	out.Header.Set("Anthropic-Beta", filterBeta(out.Header.Values("Anthropic-Beta")))
	resp, err := g.client.Do(out)
	if err != nil {
		g.refuse(w, http.StatusBadGateway, "upstream", "api_error", "Rubi: the agent gateway couldn't reach the Claude API.")
		return
	}
	defer resp.Body.Close()
	for name, vals := range resp.Header {
		if forwardedResponse(strings.ToLower(name)) {
			w.Header()[http.CanonicalHeaderKey(name)] = append([]string(nil), vals...)
		}
	}
	w.WriteHeader(resp.StatusCode)
	g.mu.Lock()
	g.served++
	g.mu.Unlock()
	// 6. stream: each chunk goes out as it arrives; a response that stalls is ended.
	rc := http.NewResponseController(w)
	stall := time.AfterFunc(gatewayStall, cancel)
	defer stall.Stop()
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(gatewayStall)
			// A guest that stops reading can't pin this goroutine and the upstream stream: the chunk must be
			// taken within gatewayWriteChunk.
			_ = rc.SetWriteDeadline(time.Now().Add(gatewayWriteChunk))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if ferr := rc.Flush(); ferr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// TokenFileOf is the token file for a sign-in directory.
func TokenFileOf(authDir string) string { return filepath.Join(authDir, "token", tokenFile) }
