package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeToken  = "sk-ant-oat01-FAKEFAKEFAKE0123456789_abcdefghij-KLMNOP"
	testSecret = "per-start-secret-0123456789abcdefghijklmnopqrstuvwxyz"
)

// upstreamRec is a stand-in for api.anthropic.com that records what it got.
type upstreamRec struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	h    http.HandlerFunc
}

type recorded struct {
	method, path, rawQuery string
	header                 http.Header
	body                   string
}

func newRecUpstream(t *testing.T, h http.HandlerFunc) *upstreamRec {
	u := &upstreamRec{h: h}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		u.mu.Unlock()
		if u.h != nil {
			u.h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("Request-Id", "req_1")
		w.Header().Set("X-Internal", "no")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRec) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

type gwEnv struct {
	g     *Gateway
	up    *upstreamRec
	token string // the token file
	logs  *strings.Builder
}

func newGW(t *testing.T, signedIn bool, secret string) *gwEnv {
	t.Helper()
	up := newRecUpstream(t, nil)
	dir := t.TempDir()
	tf := filepath.Join(dir, "token", "oauth-token")
	os.MkdirAll(filepath.Dir(tf), 0o700)
	if signedIn {
		os.WriteFile(tf, []byte(fakeToken+"\n"), 0o600)
	}
	var logs strings.Builder
	var mu sync.Mutex
	g, err := StartGateway(GatewayConfig{Secret: secret, TokenFile: tf, upstream: up.srv.URL, client: up.srv.Client(),
		Logf: func(f string, a ...any) { mu.Lock(); fmt.Fprintf(&logs, f+"\n", a...); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	return &gwEnv{g: g, up: up, token: tf, logs: &logs}
}

func (e *gwEnv) do(t *testing.T, method, path string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+e.g.Addr()+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func bearer(s string) map[string]string { return map[string]string{"Authorization": "Bearer " + s} }

func TestGatewayReplacesEveryClientCredentialWithTheRealToken(t *testing.T) {
	e := newGW(t, true, testSecret)
	resp, body := e.do(t, "POST", "/v1/messages?beta=true", map[string]string{
		"Authorization": "Bearer " + testSecret, "X-Api-Key": "sk-ant-api03-CLIENTKEYCLIENTKEYCLIENT", "Cookie": "session=abc",
		"Proxy-Authorization": "Basic Zm9vOmJhcg==", "X-Forwarded-For": "10.0.0.9", "Content-Type": "application/json",
		"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "claude-code-20250219", "X-Stainless-Lang": "js", "User-Agent": "claude-cli/2.1",
		"Anthropic-Auth-Token": "client", "X-Custom": "x",
	}, `{"model":"m"}`)
	if resp.StatusCode != 200 || body != `{"ok":true}` {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if e.up.count() != 1 {
		t.Fatalf("%d upstream requests", e.up.count())
	}
	r := e.up.reqs[0]
	if r.method != "POST" || r.path != "/v1/messages" || r.rawQuery != "beta=true" || r.body != `{"model":"m"}` {
		t.Errorf("request: %+v", r)
	}
	if got := r.header.Get("Authorization"); got != "Bearer "+fakeToken {
		t.Errorf("Authorization: %q", got)
	}
	for _, gone := range []string{"X-Api-Key", "Cookie", "Proxy-Authorization", "X-Forwarded-For", "Anthropic-Auth-Token", "X-Custom"} {
		if r.header.Get(gone) != "" {
			t.Errorf("%s was forwarded", gone)
		}
	}
	if r.header.Get("Anthropic-Version") != "2023-06-01" || r.header.Get("X-Stainless-Lang") != "js" || r.header.Get("User-Agent") != "claude-cli/2.1" {
		t.Errorf("headers: %v", r.header)
	}
	// the OAuth beta is there even when the client didn't send it
	if b := r.header.Get("Anthropic-Beta"); !strings.Contains(b, "claude-code-20250219") || !strings.Contains(b, "oauth-2025-04-20") {
		t.Errorf("beta: %q", b)
	}
	// the answer carries no cookie or internal header, and the secret and the token are in no answer
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Internal") != "" || resp.Header.Get("Request-Id") != "req_1" {
		t.Errorf("response headers: %v", resp.Header)
	}
	if strings.Contains(body, fakeToken) {
		t.Error("token in the answer")
	}
	// nothing was logged that could be a header, a body or a token
	if l := e.logs.String(); strings.Contains(l, fakeToken) || strings.Contains(l, testSecret) || strings.Contains(l, "CLIENTKEY") {
		t.Errorf("logs: %s", l)
	}
	if got := e.up.reqs[0].header.Get("Anthropic-Beta"); strings.Count(got, "oauth-2025-04-20") != 1 {
		t.Errorf("beta repeated: %s", got)
	}
}

func TestGatewayRefusesAWrongOrMissingSecret(t *testing.T) {
	e := newGW(t, true, testSecret)
	for name, hdr := range map[string]map[string]string{
		"none":            {},
		"wrong":           bearer("another-start-secret-0123456789abcdefghijklmno"),
		"the real token":  bearer(fakeToken),
		"prefix":          bearer(testSecret[:20]),
		"longer":          bearer(testSecret + "x"),
		"as an api key":   {"X-Api-Key": testSecret},
		"basic":           {"Authorization": "Basic " + testSecret},
		"lowercase":       {"Authorization": "bearer " + testSecret},
		"empty":           {"Authorization": "Bearer "},
		"secret in query": {},
	} {
		path := "/v1/messages"
		if name == "secret in query" {
			path += "?beta=true&key=" + testSecret
		}
		resp, body := e.do(t, "POST", path, hdr, "{}")
		if resp.StatusCode != 401 || !strings.Contains(body, "authentication_error") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if e.up.count() != 0 {
		t.Errorf("%d requests reached the upstream", e.up.count())
	}
	if e.g.Refusals()["bad secret"] != 10 {
		t.Errorf("refusals: %v", e.g.Refusals())
	}
}

func TestGatewayServesOnlyTheModelEndpoints(t *testing.T) {
	e := newGW(t, true, testSecret)
	h := bearer(testSecret)
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/messages", 405}, {"PUT", "/v1/messages", 405}, {"POST", "/v1/messages?foo=1", 405},
		{"POST", "/v1/messages?beta=true&x=1", 405}, {"POST", "/v1/models", 404}, {"GET", "/v1/models", 404},
		{"POST", "/api/oauth/profile", 404}, {"GET", "/api/oauth/usage", 404}, {"POST", "/api/event_logging/v2/batch", 404},
		{"POST", "/v1/messages/", 404}, {"POST", "/v1/messages/batches", 404}, {"POST", "/v1/messages/../models", 404},
		{"POST", "//v1/messages", 404}, {"POST", "/", 404}, {"POST", "/v1/files", 404},
	} {
		resp, _ := e.do(t, c.method, c.path, h, "{}")
		if resp.StatusCode != c.status {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, resp.StatusCode, c.status)
		}
	}
	for _, p := range []string{"/v1/messages", "/v1/messages?beta=true", "/v1/messages/count_tokens", "/v1/messages/count_tokens?beta=true"} {
		if resp, body := e.do(t, "POST", p, h, "{}"); resp.StatusCode != 200 {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
	if e.up.count() != 4 {
		t.Errorf("%d requests reached the upstream, want the 4 allowed ones", e.up.count())
	}
}

func TestGatewayStreamsWithoutBuffering(t *testing.T) {
	release := make(chan struct{})
	up := newRecUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: ping\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-release
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	})
	dir := t.TempDir()
	tf := filepath.Join(dir, "token", "oauth-token")
	os.MkdirAll(filepath.Dir(tf), 0o700)
	os.WriteFile(tf, []byte(fakeToken), 0o600)
	g, err := StartGateway(GatewayConfig{Secret: testSecret, TokenFile: tf, upstream: up.srv.URL, client: up.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader(`{"stream":true}`))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	first := make([]byte, 64)
	done := make(chan string, 1)
	go func() { n, _ := resp.Body.Read(first); done <- string(first[:n]) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "event: ping") {
			t.Errorf("first chunk: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first event didn't arrive before the upstream finished: the response is buffered")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "message_stop") {
		t.Errorf("rest: %q", rest)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("content type %q", resp.Header.Get("Content-Type"))
	}
}

func TestGatewayWhenNotSignedIn(t *testing.T) {
	e := newGW(t, false, testSecret)
	resp, body := e.do(t, "POST", "/v1/messages", bearer(testSecret), "{}")
	if resp.StatusCode != 401 || !strings.Contains(body, "isn't signed in to Claude") || !strings.Contains(body, "authentication_error") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	if e.up.count() != 0 {
		t.Error("a request reached the upstream without a token")
	}
	// a token appearing later is used at once, and removing it again signs the next request out
	os.WriteFile(e.token, []byte(fakeToken+"\n"), 0o600)
	if resp, _ := e.do(t, "POST", "/v1/messages", bearer(testSecret), "{}"); resp.StatusCode != 200 {
		t.Errorf("after sign-in: %d", resp.StatusCode)
	}
	os.Remove(e.token)
	if resp, _ := e.do(t, "POST", "/v1/messages", bearer(testSecret), "{}"); resp.StatusCode != 401 {
		t.Errorf("after sign-out: %d", resp.StatusCode)
	}
	// things that aren't a token file are not signed in
	for name, setup := range map[string]func(){
		"empty":     func() { os.WriteFile(e.token, nil, 0o600) },
		"no prefix": func() { os.WriteFile(e.token, []byte("hello-world-token"), 0o600) },
		"spaces":    func() { os.WriteFile(e.token, []byte("sk-ant-oat01-a b"), 0o600) },
		"huge":      func() { os.WriteFile(e.token, []byte("sk-ant-"+strings.Repeat("a", 5000)), 0o600) },
		"symlink": func() {
			real := filepath.Join(filepath.Dir(e.token), "real")
			os.WriteFile(real, []byte(fakeToken), 0o600)
			os.Symlink(real, e.token)
		},
	} {
		os.Remove(e.token)
		setup()
		if resp, _ := e.do(t, "POST", "/v1/messages", bearer(testSecret), "{}"); resp.StatusCode != 401 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
}

// The token goes to the upstream host and nowhere else: a redirect is not followed.
func TestGatewayNeverFollowsARedirect(t *testing.T) {
	elsewhere := newRecUpstream(t, nil)
	up := newRecUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/steal", http.StatusTemporaryRedirect)
	})
	dir := t.TempDir()
	tf := filepath.Join(dir, "token", "oauth-token")
	os.MkdirAll(filepath.Dir(tf), 0o700)
	os.WriteFile(tf, []byte(fakeToken), 0o600)
	g, _ := StartGateway(GatewayConfig{Secret: testSecret, TokenFile: tf, upstream: up.srv.URL, client: up.srv.Client()})
	defer g.Close()
	req, _ := http.NewRequest("POST", "http://"+g.Addr()+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+testSecret)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if elsewhere.count() != 0 {
		t.Error("the gateway followed a redirect and sent the token elsewhere")
	}
}

func TestGatewayOnlyTalksToAnthropicAndNotToEveryone(t *testing.T) {
	cfg := GatewayConfig{Secret: testSecret, TokenFile: "/nonexistent"}
	for _, up := range []string{"https://example.com", "http://api.anthropic.com", "https://api.anthropic.com.evil.example", "https://user@api.anthropic.com"} {
		cfg.upstream = up
		if g, err := StartGateway(cfg); err == nil {
			g.Close()
			t.Errorf("upstream %q accepted", up)
		}
	}
	cfg.upstream = ""
	if g, err := StartGateway(cfg); err != nil {
		t.Errorf("the default upstream: %v", err)
	} else {
		g.Close()
	}
	for _, listen := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
		c := GatewayConfig{Secret: testSecret, TokenFile: "x", Listen: listen}
		if g, err := StartGateway(c); err == nil {
			g.Close()
			t.Errorf("listening on %q accepted", listen)
		}
	}
	if _, err := StartGateway(GatewayConfig{Secret: "short"}); err == nil {
		t.Error("a short secret was accepted")
	}
}

// Two project starts get their own gateway and secret; one's secret is useless at the other.
func TestTwoGatewaysHaveDistinctSecrets(t *testing.T) {
	a := newGW(t, true, "secret-for-project-a-0123456789abcdefghijklmn")
	b := newGW(t, true, "secret-for-project-b-0123456789abcdefghijklmn")
	if resp, _ := a.do(t, "POST", "/v1/messages", bearer("secret-for-project-a-0123456789abcdefghijklmn"), "{}"); resp.StatusCode != 200 {
		t.Errorf("a with its own: %d", resp.StatusCode)
	}
	if resp, _ := b.do(t, "POST", "/v1/messages", bearer("secret-for-project-b-0123456789abcdefghijklmn"), "{}"); resp.StatusCode != 200 {
		t.Errorf("b with its own: %d", resp.StatusCode)
	}
	if resp, _ := b.do(t, "POST", "/v1/messages", bearer("secret-for-project-a-0123456789abcdefghijklmn"), "{}"); resp.StatusCode != 401 {
		t.Errorf("b with a's: %d", resp.StatusCode)
	}
	if a.g.Addr() == b.g.Addr() {
		t.Error("same address")
	}
}
