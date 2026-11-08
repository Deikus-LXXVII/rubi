package t3

// The MCP client for T3's /mcp endpoint (streamable HTTP, bearer token). Every request carries the mode
// ceiling in t3-mode-limit, so T3 itself refuses a mode above it.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolError is a tool call that T3 answered with isError.
type ToolError struct {
	Tool    string
	Message string
}

func (e *ToolError) Error() string { return "t3: " + e.Tool + ": " + e.Message }

// Client calls T3's tools. It connects on first use and once more if the server dropped the session.
type Client struct {
	base    string
	origin  *url.URL
	tok     Token
	ceiling Mode
	hc      *http.Client

	mu   sync.Mutex
	sess *mcp.ClientSession
}

const (
	mcpBodyMax     = 32 << 20 // most bytes read from one MCP response
	headerWaitMax  = 5 * time.Minute
	dialWaitMax    = 10 * time.Second
	oauthTimeout   = 30 * time.Second
	redactedClient = "t3.Client{token redacted}"
)

// New makes a client for the server at base (like http://127.0.0.1:3773). The ceiling is the highest mode
// any request through it may use.
func New(base string, tok Token, ceiling Mode) *Client {
	return NewWithTransport(base, tok, ceiling, &http.Transport{DialContext: (&net.Dialer{Timeout: dialWaitMax}).DialContext, TLSHandshakeTimeout: dialWaitMax, ResponseHeaderTimeout: headerWaitMax})
}

// NewWithTransport connects through a supervisor-owned transport (e.g. inherited
// project pipes). Authentication still has exactly the same origin/mode guard.
func NewWithTransport(base string, tok Token, ceiling Mode, rt http.RoundTripper) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), tok: tok, ceiling: ceiling.norm()}
	c.origin, _ = url.Parse(c.base)
	c.hc = &http.Client{Transport: headerTransport{c, rt}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return c
}

// String and GoString keep the token out of a print of the client (its fields are unexported, so fmt would
// show the raw token otherwise).
func (c *Client) String() string   { return redactedClient }
func (c *Client) GoString() string { return redactedClient }

// headerTransport adds the bearer token and the mode limit to each request, to the base server only.
type headerTransport struct {
	c  *Client
	rt http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if h.c.origin == nil || r.URL.Scheme != h.c.origin.Scheme || r.URL.Host != h.c.origin.Host {
		return nil, errors.New("t3: refusing a request to another server than the T3 base")
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.c.tok.Secret())
	r.Header.Set("t3-mode-limit", ModeLimit(h.c.ceiling))
	resp, err := h.rt.RoundTrip(r)
	if err == nil {
		resp.Body = capBody(resp.Body, mcpBodyMax)
	}
	return resp, err
}

// capBody stops reading a body after max bytes.
func capBody(b io.ReadCloser, max int64) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(b, max), b}
}

// ModeLimit is the t3-mode-limit header value for a ceiling: T3 wants <runtimeMode>/<interactionMode>, and
// the interaction mode stays at its default (a thread may still plan).
func ModeLimit(ceiling Mode) string { return string(ceiling.norm()) + "/default" }

// Close ends the session, if there is one.
func (c *Client) Close() error {
	c.mu.Lock()
	s := c.sess
	c.sess = nil
	c.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}

func (c *Client) session(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil {
		return s, nil
	}
	// Connect without the lock: it is a network round trip.
	cl := mcp.NewClient(&mcp.Implementation{Name: "rubi-home", Version: "0"}, nil)
	s, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             c.base + "/mcp",
		HTTPClient:           c.hc,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		return nil, errors.New("t3: connect: " + err.Error())
	}
	c.mu.Lock()
	if c.sess != nil { // another caller connected first
		other := c.sess
		c.mu.Unlock()
		_ = s.Close()
		return other, nil
	}
	c.sess = s
	c.mu.Unlock()
	return s, nil
}

func (c *Client) drop(s *mcp.ClientSession) {
	c.mu.Lock()
	if c.sess == s {
		c.sess = nil
	}
	c.mu.Unlock()
	_ = s.Close()
}

// lost reports a failure that means the session is gone, so a new one may help.
func lost(err error) bool {
	return errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)
}

// raw calls a tool and returns its result, reconnecting once if the session was lost.
func (c *Client) raw(ctx context.Context, tool string, args any) (*mcp.CallToolResult, error) {
	var err error
	for try := 0; try < 2; try++ {
		var s *mcp.ClientSession
		if s, err = c.session(ctx); err != nil {
			return nil, err
		}
		var res *mcp.CallToolResult
		res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		if err == nil {
			return res, nil
		}
		if !lost(err) || ctx.Err() != nil {
			break
		}
		c.drop(s)
	}
	return nil, errors.New("t3: " + tool + ": " + err.Error())
}

// call runs a tool and decodes its structured result into out (nil to ignore it).
func (c *Client) call(ctx context.Context, tool string, args, out any) error {
	res, err := c.raw(ctx, tool, args)
	if err != nil {
		return err
	}
	if res.IsError {
		return &ToolError{Tool: tool, Message: textOf(res)}
	}
	if out == nil {
		return nil
	}
	var b []byte
	if res.StructuredContent != nil {
		b, err = json.Marshal(res.StructuredContent)
	} else if t := textOf(res); t != "" {
		b = []byte(t)
	} else {
		return errors.New("t3: " + tool + ": empty result")
	}
	if err == nil {
		err = json.Unmarshal(b, out)
	}
	if err != nil {
		return errors.New("t3: " + tool + ": result is not what was expected")
	}
	return nil
}

func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Tools lists the server's tools, for the contract check.
func (c *Client) Tools(ctx context.Context) ([]*mcp.Tool, error) {
	s, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	var out []*mcp.Tool
	for t, err := range s.Tools(ctx, nil) {
		if err != nil {
			return nil, errors.New("t3: tools/list: " + err.Error())
		}
		out = append(out, t)
	}
	return out, nil
}
