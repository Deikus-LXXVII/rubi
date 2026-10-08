// Package homeproto is how Rubi talks to Rubi Home, a small helper on a computer at the user's home (for
// example a Mac that stays on). Rubi runs on the agent's machine and can't reach the home network; the
// helper can. The helper is the server: it keeps a fixed routing key on the relays (Rubi Gateway and
// public ones) and its own identity key, and Rubi calls it with the same end-to-end encryption the panel
// uses with Rubi. A random token, given to the helper when it was paired, proves the caller is the
// user's Rubi.
//
// Pairing: the helper prints a one-time code (its routing key, its public identity, the relays and a
// secret); the user pastes it into the Rubi panel; Rubi calls "pair" with that secret and a new token.
package homeproto

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

// Path binds requests to this protocol (it is part of the encryption's associated data).
const Path = "/home/v1/rpc"

const clockSkew = 2 * time.Minute

var b64 = base64.RawURLEncoding

// Envelope is a request inside the encryption.
type Envelope struct {
	Op    string          `json:"op"`
	TS    int64           `json:"ts"`
	RID   string          `json:"rid"`
	Token string          `json:"token,omitempty"`
	Args  json.RawMessage `json:"args,omitempty"`
}

type reply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Code is what the helper shows for pairing.
type Code struct {
	Relay  string   `json:"n"`           // routing key on the relays
	Bundle string   `json:"k"`           // public identity
	Relays []string `json:"r,omitempty"` // relays it listens on ("" = the defaults)
	Secret string   `json:"c"`           // one-time pairing secret
	Name   string   `json:"m,omitempty"` // e.g. the computer's name
}

const codePrefix = "rubi-home:"

func (c Code) String() string {
	b, _ := json.Marshal(c)
	return codePrefix + b64.EncodeToString(b)
}

// ParseCode reads a pairing code (surrounding spaces and line breaks are ignored).
func ParseCode(s string) (Code, error) {
	s = strings.Join(strings.Fields(s), "")
	var c Code
	if !strings.HasPrefix(s, codePrefix) {
		return c, errors.New("not a Rubi Home pairing code (it starts with rubi-home:)")
	}
	b, err := b64.DecodeString(strings.TrimPrefix(s, codePrefix))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Relay == "" || c.Bundle == "" || c.Secret == "" {
		return c, errors.New("this pairing code is incomplete; copy the whole line")
	}
	if _, err := e2e.ParseInstanceBundle(c.Bundle); err != nil {
		return c, errors.New("this pairing code is damaged; copy it again")
	}
	return c, nil
}

// RandomToken returns 32 random bytes, base64url.
func RandomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b64.EncodeToString(b)
}

// ---- client (Rubi's side) ----

// Client calls one helper. It keeps its relay connection open between calls.
type Client struct {
	Relay  string
	Relays []string
	Token  string

	ch e2e.Client
	mu sync.Mutex
	rc *relay.Client
}

func NewClient(relayKey, bundle string, relays []string, token string) (*Client, error) {
	pub, err := e2e.ParseInstanceBundle(bundle)
	if err != nil {
		return nil, err
	}
	return &Client{Relay: relayKey, Relays: relays, Token: token, ch: e2e.Client{InstanceKey: pub}}, nil
}

func (c *Client) conn(ctx context.Context) (*relay.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc != nil {
		return c.rc, nil
	}
	relays := c.Relays
	if len(relays) == 0 {
		relays = DefaultRelays()
	}
	rc, err := relay.Dial(ctx, c.Relay, relays)
	if err != nil {
		return nil, fmt.Errorf("can't reach the relays: %w", err)
	}
	c.rc = rc
	return rc, nil
}

// Close drops the relay connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rc != nil {
		c.rc.Close()
		c.rc = nil
	}
}

// ErrOffline means the helper didn't answer (the computer is off, asleep or offline).
var ErrOffline = errors.New("Rubi Home didn't answer; is the computer at home on and online?")

// Call performs one encrypted request.
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	rid := make([]byte, 16)
	_, _ = rand.Read(rid)
	env := Envelope{Op: op, TS: time.Now().Unix(), RID: b64.EncodeToString(rid), Token: c.Token, Args: raw}
	plain, _ := json.Marshal(env)
	req, respKey, err := c.ch.Seal(Path, plain)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(req)
	rc, err := c.conn(ctx)
	if err != nil {
		return err
	}
	status, resp, err := rc.Do(ctx, body)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return ErrOffline
		}
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("Rubi Home answered %d", status)
	}
	var sealed e2e.Response
	if err := json.Unmarshal(resp, &sealed); err != nil {
		return err
	}
	opened, err := e2e.OpenResponse(respKey, env.RID, sealed)
	if err != nil {
		return errors.New("the answer didn't come from your Rubi Home")
	}
	var r reply
	if err := json.Unmarshal(opened, &r); err != nil {
		return err
	}
	if !r.OK {
		return errors.New(r.Error)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// DefaultRelays are Rubi Gateway plus the public relays.
func DefaultRelays() []string {
	return append([]string{relay.GatewayURL}, relay.DefaultRelays...)
}

// ---- server (the helper's side) ----

// Server opens requests, rejects replays and stale ones, and seals the answers.
type Server struct {
	Key    *ecdh.PrivateKey
	Handle func(ctx context.Context, env Envelope) (any, error)

	mu   sync.Mutex
	seen map[string]time.Time
}

func (s *Server) fresh(env Envelope) error {
	now := time.Now()
	ts := time.Unix(env.TS, 0)
	if ts.Before(now.Add(-clockSkew)) || ts.After(now.Add(clockSkew)) {
		return errors.New("the request time is off by more than 2 minutes; check the clocks")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	for rid, exp := range s.seen {
		if now.After(exp) {
			delete(s.seen, rid)
		}
	}
	if _, dup := s.seen[env.RID]; dup || env.RID == "" {
		return errors.New("replayed request")
	}
	s.seen[env.RID] = now.Add(2 * clockSkew)
	return nil
}

// HandleRPC serves one encrypted request (plug it into relay.Server.Handle).
func (s *Server) HandleRPC(ctx context.Context, body []byte) (int, []byte) {
	var req e2e.Request
	if len(body) > 1<<20 || json.Unmarshal(body, &req) != nil {
		return http.StatusBadRequest, []byte("bad request")
	}
	plain, respKey, err := e2e.Server{Key: s.Key}.Open(Path, req)
	if err != nil {
		return http.StatusBadRequest, []byte("bad request")
	}
	var env Envelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return http.StatusBadRequest, []byte("bad request")
	}
	var out reply
	if err := s.fresh(env); err != nil {
		out.Error = err.Error()
	} else if res, err := s.Handle(ctx, env); err != nil {
		out.Error = err.Error()
	} else {
		out.OK = true
		out.Result, _ = json.Marshal(res)
	}
	plainOut, _ := json.Marshal(out)
	sealed, err := e2e.SealResponse(respKey, env.RID, plainOut)
	if err != nil {
		return http.StatusInternalServerError, nil
	}
	b, _ := json.Marshal(sealed)
	return http.StatusOK, b
}
