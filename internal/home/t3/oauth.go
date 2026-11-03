package t3

// Sign-in to T3's MCP endpoint. A pairing code minted on the host (t3 auth pairing create) is traded for an
// access token through T3's OAuth flow: discovery, dynamic registration with a loopback redirect, PKCE (S256),
// the pairing-code decision, and the token exchange. Nothing listens on the redirect.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	redirectURI   = "http://127.0.0.1:1/cb"
	oauthBodyMax  = 1 << 20
	redactedToken = "[redacted token]"
)

// Token is what a sign-in gives. The access token is kept out of every print, log line and encoding; use
// Save to write it to disk and Secret to send it.
type Token struct {
	Access   string
	Scopes   []string
	Expires  time.Time
	ClientID string
}

// String, GoString and MarshalText redact the access token.
func (t Token) String() string   { return t.redacted() }
func (t Token) GoString() string { return t.redacted() }

// MarshalText redacts the access token.
func (t Token) MarshalText() ([]byte, error) { return []byte(t.redacted()), nil }

func (t Token) redacted() string {
	return fmt.Sprintf("Token{%s scopes=%v expires=%s client=%s}", redactedToken, t.Scopes, t.Expires.UTC().Format(time.RFC3339), t.ClientID)
}

// Secret is the access token itself.
func (t Token) Secret() string { return t.Access }

type savedToken struct {
	Access   string    `json:"access_token"`
	Scopes   []string  `json:"scopes"`
	Expires  time.Time `json:"expires"`
	ClientID string    `json:"client_id"`
}

// Save encodes the token, access token included, for a private file.
func (t Token) Save() ([]byte, error) {
	return json.Marshal(savedToken{t.Access, t.Scopes, t.Expires, t.ClientID})
}

// LoadToken reads what Save wrote.
func LoadToken(b []byte) (Token, error) {
	var s savedToken
	if err := json.Unmarshal(b, &s); err != nil {
		return Token{}, errors.New("t3: saved token is not readable")
	}
	if s.Access == "" {
		return Token{}, errors.New("t3: saved token is empty")
	}
	return Token{s.Access, s.Scopes, s.Expires, s.ClientID}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b64(b), nil
}

// oauthCall sends one request and returns the body of a 2xx answer (registration answers 201). Any other status becomes an error with
// T3's message, with secrets cut out of it.
func oauthCall(hc *http.Client, req *http.Request, secrets ...string) ([]byte, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t3: %s: %w", req.URL.Path, scrub(err.Error(), secrets))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, oauthBodyMax))
	if err != nil {
		return nil, fmt.Errorf("t3: %s: read: %w", req.URL.Path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("t3: %s: %d %s", req.URL.Path, resp.StatusCode, scrub(errorMessage(body), secrets))
	}
	return body, nil
}

func scrub(s string, secrets []string) error {
	for _, v := range secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return errors.New(s)
}

func errorMessage(body []byte) string {
	var j struct {
		Message          string `json:"message"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if json.Unmarshal(body, &j) == nil {
		for _, m := range []string{j.ErrorDescription, j.Message, j.Error} {
			if m != "" {
				return m
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func postJSON(ctx context.Context, hc *http.Client, u string, v any, secrets ...string) ([]byte, error) {
	buf, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return oauthCall(hc, req, secrets...)
}

// noRedirects copies the caller's client (or makes one with a timeout) and stops it from following
// redirects: a 307 or 308 would send the body, with the pairing code or the verifier, to another address.
func noRedirects(hc *http.Client) *http.Client {
	c := &http.Client{Timeout: oauthTimeout}
	if hc != nil {
		cp := *hc
		c = &cp
		if c.Timeout == 0 {
			c.Timeout = oauthTimeout
		}
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// Pair signs in to the T3 server at base (like http://127.0.0.1:3773) with a pairing code for the given MCP
// access level and returns the token. The code and the token never appear in an error.
func Pair(ctx context.Context, hc *http.Client, base, code, access, clientName string) (Token, error) {
	hc = noRedirects(hc)
	base = strings.TrimRight(base, "/")
	resource := base + "/mcp"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return Token{}, err
	}
	body, err := oauthCall(hc, req, code)
	if err != nil {
		return Token{}, err
	}
	var meta struct {
		Issuer        string `json:"issuer"`
		Registration  string `json:"registration_endpoint"`
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return Token{}, errors.New("t3: discovery answer is not JSON")
	}
	if meta.Issuer != base {
		return Token{}, errors.New("t3: the server names a different issuer")
	}
	for _, e := range []string{meta.Registration, meta.TokenEndpoint} {
		if !strings.HasPrefix(e, base+"/") {
			return Token{}, errors.New("t3: the server points outside itself")
		}
	}

	body, err = postJSON(ctx, hc, meta.Registration, map[string]any{"client_name": clientName, "redirect_uris": []string{redirectURI}}, code)
	if err != nil {
		return Token{}, err
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if json.Unmarshal(body, &reg) != nil || reg.ClientID == "" {
		return Token{}, errors.New("t3: registration gave no client id")
	}

	verifier, err := randomString()
	if err != nil {
		return Token{}, err
	}
	state, err := randomString()
	if err != nil {
		return Token{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	body, err = postJSON(ctx, hc, base+"/oauth/mcp/decision", map[string]any{
		"authorization": map[string]string{
			"response_type": "code", "client_id": reg.ClientID, "redirect_uri": redirectURI,
			"code_challenge": b64(sum[:]), "code_challenge_method": "S256", "state": state, "resource": resource,
		},
		"decision": map[string]string{"_tag": "pairing-code", "access": access, "code": code},
	}, code, verifier)
	if err != nil {
		return Token{}, err
	}
	var dec struct {
		RedirectTo string `json:"redirectTo"`
	}
	if json.Unmarshal(body, &dec) != nil {
		return Token{}, errors.New("t3: decision answer is not JSON")
	}
	ru, err := url.Parse(dec.RedirectTo)
	if err != nil {
		return Token{}, errors.New("t3: decision gave a bad redirect")
	}
	q := ru.Query()
	if q.Get("state") != state {
		return Token{}, errors.New("t3: state did not come back")
	}
	if q.Get("iss") != meta.Issuer {
		return Token{}, errors.New("t3: the answer is not from the issuer")
	}
	authCode := q.Get("code")
	if authCode == "" {
		return Token{}, errors.New("t3: decision gave no code")
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "code_verifier": {verifier},
		"redirect_uri": {redirectURI}, "client_id": {reg.ClientID}, "resource": {resource}}
	treq, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	treq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	body, err = oauthCall(hc, treq, code, authCode, verifier)
	if err != nil {
		return Token{}, err
	}
	var tok struct {
		Access    string `json:"access_token"`
		Scope     string `json:"scope"`
		ExpiresIn int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.Access == "" {
		return Token{}, errors.New("t3: token answer has no access token")
	}
	t := Token{Access: tok.Access, Scopes: strings.Fields(tok.Scope), ClientID: reg.ClientID}
	if tok.ExpiresIn > 0 {
		t.Expires = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	return t, nil
}
