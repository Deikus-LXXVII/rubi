// Package panelapi is the HTTP API the panel talks to, reached through the public tunnel.
//
// There is a single encrypted endpoint, POST /v1/rpc. The operation name and everything else live inside
// the end-to-end encrypted envelope (see package e2e), so the tunnel provider learns nothing but sizes
// and timing. Beyond "hello", every operation needs either the pairing code or a ticket from a link that
// Rubi generated for the user.
package panelapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

const (
	RPCPath     = "/v1/rpc"
	maxBody     = 256 << 10
	clockSkew   = 2 * time.Minute
	failWindow  = 10 * time.Minute
	maxFailures = 20
)

// Envelope is the decrypted request body.
type Envelope struct {
	Op     string          `json:"op"`
	TS     int64           `json:"ts"`  // unix seconds
	RID    string          `json:"rid"` // random request id, single use
	Ticket string          `json:"ticket,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type reply struct {
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type Server struct {
	core *core.Core
	ch   e2e.Server
	now  func() time.Time

	mu       sync.Mutex
	seen     map[string]time.Time // request ids within the replay window
	failures []time.Time
}

func New(c *core.Core) *Server {
	return &Server{core: c, ch: e2e.Server{Key: c.ID.Box}, now: time.Now, seen: map[string]time.Time{}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		s.cors(w, r)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("OPTIONS "+RPCPath, func(w http.ResponseWriter, r *http.Request) {
		s.cors(w, r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+RPCPath, s.rpc)
	return mux
}

// cors allows only the configured panel origin. Requests without an Origin (non-browser clients) are
// unaffected: the encryption, not CORS, is what protects the API.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" && o == s.core.PanelOrigin {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		h.Set("Vary", "Origin")
	}
}

func (s *Server) rpc(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	w.Header().Set("Cache-Control", "no-store")
	var req e2e.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	plain, respKey, err := s.ch.Open(RPCPath, req)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var env Envelope
	if err := json.Unmarshal(plain, &env); err != nil || env.RID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var out reply
	if err := s.checkFresh(env); err != nil {
		out = reply{Error: err.Error()}
	} else {
		res, err := s.dispatch(r.Context(), env)
		if err != nil {
			out = reply{Error: err.Error()}
		} else {
			out = reply{OK: true, Result: res}
		}
	}
	body, _ := json.Marshal(out)
	sealed, err := e2e.SealResponse(respKey, env.RID, body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sealed)
}

// checkFresh rejects replays of a captured request and stale requests.
func (s *Server) checkFresh(env Envelope) error {
	now := s.now()
	ts := time.Unix(env.TS, 0)
	if ts.Before(now.Add(-clockSkew)) || ts.After(now.Add(clockSkew)) {
		return errors.New("request time is off by more than 2 minutes; check the device clock")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for rid, exp := range s.seen {
		if now.After(exp) {
			delete(s.seen, rid)
		}
	}
	if _, dup := s.seen[env.RID]; dup {
		return errors.New("replayed request")
	}
	s.seen[env.RID] = now.Add(2 * clockSkew)
	return nil
}

func (s *Server) throttled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-failWindow)
	kept := s.failures[:0]
	for _, t := range s.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.failures = kept
	return len(s.failures) >= maxFailures
}

func (s *Server) fail(err error) error {
	s.mu.Lock()
	s.failures = append(s.failures, s.now())
	s.mu.Unlock()
	return err
}

var errThrottled = errors.New("too many failed attempts; wait 10 minutes")

func (s *Server) dispatch(ctx context.Context, env Envelope) (any, error) {
	switch env.Op {
	case "hello":
		return map[string]any{"instance": s.core.ID.InstanceID, "fingerprint": s.core.ID.Fingerprint(),
			"state": s.core.State(), "version": version.Version}, nil
	case "pair":
		return s.pair(env)
	}

	if s.throttled() {
		return nil, errThrottled
	}
	purpose, ok := s.core.TicketPurpose(env.Ticket)
	if !ok {
		return nil, s.fail(errors.New("this link has expired; ask your agent for a new one"))
	}
	switch env.Op {
	case "unlock.keys":
		keys, err := s.core.Keys()
		if err != nil {
			return nil, err
		}
		return map[string]any{"instance": keys.Instance, "wraps": keys.Wraps, "purpose": purpose}, nil
	case "unlock":
		return s.unlock(env)
	case "status":
		return s.status(purpose), nil
	case "lock":
		s.core.Lock()
		return map[string]any{"state": s.core.State()}, nil
	}
	return nil, fmt.Errorf("unknown operation %q", env.Op)
}

type pairArgs struct {
	Code         string           `json:"code"`
	DEK          string           `json:"dek"`
	Wraps        []vault.Wrap     `json:"wraps"`
	Approvers    []vault.Approver `json:"approvers,omitempty"`
	Locale       string           `json:"locale,omitempty"`
	Method       string           `json:"method"`
	CredentialID string           `json:"credential_id,omitempty"`
}

func (s *Server) pair(env Envelope) (any, error) {
	if s.throttled() {
		return nil, errThrottled
	}
	var a pairArgs
	if err := json.Unmarshal(env.Args, &a); err != nil {
		return nil, errors.New("bad arguments")
	}
	if s.core.State() != core.Unpaired {
		return nil, errors.New("Rubi is already paired")
	}
	if !s.core.CheckPairingCode(a.Code) {
		return nil, s.fail(errors.New("this pairing link has expired; ask your agent for a new one"))
	}
	dek, err := base64.RawURLEncoding.DecodeString(a.DEK)
	if err != nil || len(dek) != vault.KeySize {
		return nil, errors.New("bad data key")
	}
	if len(a.Wraps) == 0 {
		return nil, errors.New("at least one passkey or password is required")
	}
	for i := range a.Wraps {
		if a.Wraps[i].CreatedAt.IsZero() {
			a.Wraps[i].CreatedAt = time.Now().UTC()
		}
	}
	data := vault.NewData("")
	data.Approvers, data.Locale = a.Approvers, a.Locale
	if err := s.core.Pair(dek, &vault.Keys{Wraps: a.Wraps}, data, a.Method, a.CredentialID); err != nil {
		return nil, err
	}
	v, _ := s.core.Vault.Version()
	return map[string]any{"state": s.core.State(), "vault_version": v}, nil
}

type unlockArgs struct {
	DEK             string `json:"dek"`
	MinVaultVersion uint64 `json:"min_vault_version"`
	Method          string `json:"method"`
	CredentialID    string `json:"credential_id,omitempty"`
}

func (s *Server) unlock(env Envelope) (any, error) {
	var a unlockArgs
	if err := json.Unmarshal(env.Args, &a); err != nil {
		return nil, errors.New("bad arguments")
	}
	if s.core.State() == core.Unlocked {
		v, _ := s.core.Vault.Version()
		return map[string]any{"state": core.Unlocked, "vault_version": v}, nil
	}
	dek, err := base64.RawURLEncoding.DecodeString(a.DEK)
	if err != nil || len(dek) != vault.KeySize {
		return nil, s.fail(errors.New("bad data key"))
	}
	if err := s.core.Unlock(dek, a.MinVaultVersion, a.Method, a.CredentialID); err != nil {
		if errors.Is(err, vault.ErrVersion) {
			return nil, errors.New("the stored data is older than what this device saw before; " +
				"it may have been rolled back. Rubi stays locked")
		}
		return nil, s.fail(errors.New("unlock failed"))
	}
	v, _ := s.core.Vault.Version()
	return map[string]any{"state": s.core.State(), "vault_version": v}, nil
}

func (s *Server) status(purpose string) any {
	out := map[string]any{"state": s.core.State(), "purpose": purpose, "instance": s.core.ID.InstanceID}
	_ = s.core.Vault.View(func(d *vault.Data) error {
		out["vault_version"] = d.Version
		out["receipts"] = d.Receipts
		installed := map[string]bool{}
		for id, i := range d.Integrations {
			installed[id] = i.Enabled
		}
		out["integrations"] = installed
		return nil
	})
	return out
}
