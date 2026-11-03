package t3

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	fakeCode  = "PAIR-CODE-7Q2X"
	fakeToken = "tok-secret-9f3a"
)

// call is one tool call the stand-in saw.
type call struct {
	Tool   string
	Args   map[string]any
	Header http.Header
}

// fakeT3 stands in for a T3 server: the OAuth endpoints (verifying PKCE, state and resource) and an MCP
// endpoint whose tools record what they get and answer from reply.
type fakeT3 struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	calls     []call
	sessions  map[string]bool // the session ids clients used
	dropOnce  bool
	challenge string
	state     string
	clientID  string
	reply     map[string]func(args map[string]any) (any, string) // structured result, or an error message

	// knobs for the sign-in failures
	issuer       string // overrides the advertised issuer
	badState     bool
	badIss       bool
	foreignToken bool

	redirectMCP      string // /mcp answers 307 to this URL
	redirectDecision string // the decision endpoint answers 307 to this URL
	droppedID        string // the clientRequestId of the request a dropped session lost
}

func newFake(t *testing.T, tools ...string) *fakeT3 {
	f := &fakeT3{t: t, reply: map[string]func(map[string]any) (any, string){}, sessions: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", f.locked(f.discovery))
	mux.HandleFunc("/oauth/mcp/register", f.locked(f.register))
	mux.HandleFunc("/oauth/mcp/decision", f.locked(f.decision))
	mux.HandleFunc("/oauth/mcp/token", f.locked(f.token))

	s := mcp.NewServer(&mcp.Implementation{Name: "fake-t3", Version: "0"}, nil)
	for _, name := range tools {
		name := name
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				f.mu.Lock()
				f.calls = append(f.calls, call{name, args, req.Extra.Header.Clone()})
				fn := f.reply[name]
				f.mu.Unlock()
				if fn == nil {
					return &mcp.CallToolResult{StructuredContent: map[string]any{}, Content: []mcp.Content{&mcp.TextContent{Text: "{}"}}}, nil
				}
				out, msg := fn(args)
				if msg != "" {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil
				}
				return &mcp.CallToolResult{StructuredContent: out, Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		if id := r.Header.Get("Mcp-Session-Id"); id != "" {
			f.sessions[id] = true
		}
		redirect := f.redirectMCP
		drop := f.dropOnce && r.Header.Get("Mcp-Session-Id") != ""
		if drop {
			f.dropOnce = false
		}
		f.mu.Unlock()
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusTemporaryRedirect)
			return
		}
		if drop {
			// The request reached the server before the session was lost.
			var body struct {
				Params struct {
					Arguments struct {
						ID string `json:"clientRequestId"`
					} `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.droppedID = body.Params.Arguments.ID
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// locked serializes a handler with the test's changes to the knobs.
func (f *fakeT3) locked(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		h(w, r)
	}
}

// set changes the stand-in's state between requests.
func (f *fakeT3) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeT3) base() string { return f.srv.URL }

func (f *fakeT3) discovery(w http.ResponseWriter, r *http.Request) {
	iss := f.srv.URL
	if f.issuer != "" {
		iss = f.issuer
	}
	reg := f.srv.URL + "/oauth/mcp/register"
	tok := f.srv.URL + "/oauth/mcp/token"
	if f.foreignToken {
		tok = "http://127.0.0.1:9/token"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss, "registration_endpoint": reg, "token_endpoint": tok,
		"code_challenge_methods_supported": []string{"S256"}})
}

func (f *fakeT3) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string   `json:"client_name"`
		URIs []string `json:"redirect_uris"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Name == "" || len(in.URIs) != 1 {
		http.Error(w, `{"message":"bad registration"}`, http.StatusBadRequest)
		return
	}
	f.clientID = "client-1"
	_ = json.NewEncoder(w).Encode(map[string]string{"client_id": f.clientID})
}

func (f *fakeT3) decision(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Authorization map[string]string `json:"authorization"`
		Decision      map[string]string `json:"decision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if f.redirectDecision != "" {
		http.Redirect(w, r, f.redirectDecision, http.StatusTemporaryRedirect)
		return
	}
	a := in.Authorization
	switch {
	case in.Decision["_tag"] != "pairing-code":
		http.Error(w, `{"message":"unsupported decision"}`, http.StatusBadRequest)
		return
	case in.Decision["code"] != fakeCode:
		// A server may echo what it was sent; Pair must not pass that on.
		b, _ := json.Marshal(map[string]string{"message": "Pairing code " + in.Decision["code"] + " is not valid"})
		http.Error(w, string(b), http.StatusForbidden)
		return
	case a["response_type"] != "code" || a["code_challenge_method"] != "S256" || a["client_id"] != f.clientID ||
		a["code_challenge"] == "" || a["state"] == "" || a["resource"] != f.srv.URL+"/mcp":
		http.Error(w, `{"message":"bad authorization"}`, http.StatusBadRequest)
		return
	}
	f.challenge, f.state = a["code_challenge"], a["state"]
	q := url.Values{"code": {"auth-code-1"}, "state": {a["state"]}, "iss": {f.srv.URL}}
	if f.badState {
		q.Set("state", "other")
	}
	if f.badIss {
		q.Set("iss", "http://evil.invalid")
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"redirectTo": a["redirect_uri"] + "?" + q.Encode()})
}

func (f *fakeT3) token(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.ParseForm() != nil {
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "auth-code-1" ||
		b64(sum[:]) != f.challenge || r.PostForm.Get("client_id") != f.clientID ||
		r.PostForm.Get("resource") != f.srv.URL+"/mcp" || r.PostForm.Get("redirect_uri") == "" {
		http.Error(w, `{"message":"invalid grant"}`, http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeToken, "token_type": "Bearer", "expires_in": 3600,
		"scope": "orchestration:read orchestration:operate"})
}

func (f *fakeT3) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		f.t.Fatal("no tool call reached the server")
	}
	return f.calls[len(f.calls)-1]
}

// lastOf is the last call of one tool.
func (f *fakeT3) lastOf(tool string) call {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].Tool == tool {
			return f.calls[i]
		}
	}
	f.t.Fatalf("no %s call reached the server", tool)
	return call{}
}

func (f *fakeT3) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// client signs in against the stand-in and returns a client with the given ceiling.
func (f *fakeT3) client(ceiling Mode) *Client {
	f.t.Helper()
	c := New(f.base(), Token{Access: fakeToken}, ceiling)
	f.t.Cleanup(func() { _ = c.Close() })
	return c
}

var allTools = UsedTools
