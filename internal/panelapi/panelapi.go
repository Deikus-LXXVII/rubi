// Package panelapi is the HTTP API the panel talks to, reached through the public tunnel.
//
// There is a single encrypted endpoint, POST /v1/rpc. The operation name and everything else live inside
// the end-to-end encrypted envelope (see package e2e), so the tunnel provider learns nothing but sizes
// and timing. Beyond "hello", every operation needs either the pairing code or a ticket from a link that
// Rubi generated for the user.
package panelapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/version"
	"github.com/Deikus-LXXVII/rubi/internal/webauthn"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
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
	// Hook addresses, when Rubi is reached directly (Tailscale); through Rubi Gateway they arrive as events.
	mux.HandleFunc("POST /h/{route}/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !s.core.DeliverHookAt(r.PathValue("route"), r.PathValue("id"), body) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
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
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	status, resp := s.HandleRPC(r.Context(), body)
	if status != http.StatusOK {
		http.Error(w, string(resp), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

// HandleRPC processes one encrypted request body, whatever carried it (HTTP or relays), and returns the
// encrypted response body.
func (s *Server) HandleRPC(ctx context.Context, body []byte) (int, []byte) {
	bad := []byte("bad request")
	if len(body) > maxBody {
		return http.StatusBadRequest, bad
	}
	var req e2e.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return http.StatusBadRequest, bad
	}
	plain, respKey, err := s.ch.Open(RPCPath, req)
	if err != nil {
		return http.StatusBadRequest, bad
	}
	var env Envelope
	if err := json.Unmarshal(plain, &env); err != nil || env.RID == "" {
		return http.StatusBadRequest, bad
	}
	var out reply
	if err := s.checkFresh(env); err != nil {
		out = reply{Error: err.Error()}
	} else {
		res, err := s.dispatch(ctx, env)
		if err != nil {
			out = reply{Error: err.Error()}
		} else {
			out = reply{OK: true, Result: res}
		}
	}
	plainOut, _ := json.Marshal(out)
	sealed, err := e2e.SealResponse(respKey, env.RID, plainOut)
	if err != nil {
		return http.StatusInternalServerError, []byte("internal error")
	}
	resp, _ := json.Marshal(sealed)
	return http.StatusOK, resp
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
	case "updates.list", "updates.notify", "updates.rubi", "updates.plugin":
		// The panel shows the Updates button on every screen, so any valid link may use it. Listing and
		// notification switches are harmless; installing an update still needs a strong approval.
		return s.updates(ctx, env)
	case "approval.get":
		return s.approvalGet(purpose, env)
	case "approval.decide":
		return s.approvalDecide(ctx, purpose, env)
	case "approval.challenge": // the challenge for a chosen subset of a batch approval
		var a struct {
			ApprovalID string `json:"approval_id"`
			Option     string `json:"option"`
		}
		if err := json.Unmarshal(env.Args, &a); err != nil {
			return nil, errors.New("bad arguments")
		}
		if err := approvalTicket(purpose, a.ApprovalID); err != nil {
			return nil, err
		}
		if err := s.core.Approvals.CheckOption(a.ApprovalID, a.Option); err != nil {
			return nil, err
		}
		d, err := s.core.Approvals.Detail(a.ApprovalID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"challenge": base64.RawURLEncoding.EncodeToString(s.challenge(d, a.Option))}, nil
	case "integration.catalog", "integration.setup", "integration.disconnect",
		"policy.get", "policy.set", "webhook.get", "webhook.set", "webhook.test",
		"store.list", "plugin.install", "plugin.update", "plugin.remove", "plugin.rollback",
		"agents.get", "agent.add", "agent.remove", "agent.default", "agent.subscribe",
		"plugin.config.get", "plugin.config.set":
		return s.settings(ctx, purpose, env)
	}
	return nil, fmt.Errorf("unknown operation %q", env.Op)
}

type pairArgs struct {
	Code               string           `json:"code"`
	DEK                string           `json:"dek"`
	Wraps              []vault.Wrap     `json:"wraps"`
	Approvers          []vault.Approver `json:"approvers,omitempty"`
	PasswordApproveKey string           `json:"password_approve_key,omitempty"`
	Locale             string           `json:"locale,omitempty"`
	Method             string           `json:"method"`
	CredentialID       string           `json:"credential_id,omitempty"`
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
	if a.PasswordApproveKey != "" {
		k, err := base64.RawURLEncoding.DecodeString(a.PasswordApproveKey)
		if err != nil || len(k) != 32 {
			return nil, errors.New("bad password approval key")
		}
		data.PasswordApproveKey = k
	}
	for _, ap := range a.Approvers {
		if ap.CredentialID == "" || len(ap.PublicKey) == 0 {
			return nil, errors.New("bad passkey registration")
		}
	}
	if err := s.core.Pair(dek, &vault.Keys{Wraps: a.Wraps}, data, a.Method, a.CredentialID); err != nil {
		return nil, err
	}
	v, _ := s.core.Vault.Version()
	// The panel continues straight to connecting the agent's webhook, which needs a settings ticket.
	return map[string]any{"state": s.core.State(), "vault_version": v, "ticket": s.core.MintTicket("settings")}, nil
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
	// The panel continues straight to connecting the agent's webhook, which needs a settings ticket.
	return map[string]any{"state": s.core.State(), "vault_version": v, "ticket": s.core.MintTicket("settings")}, nil
}

func (s *Server) status(purpose string) any {
	out := map[string]any{"state": s.core.State(), "purpose": purpose, "instance": s.core.ID.InstanceID,
		"version": version.Version, "integrity": s.core.Integrity(), "update": s.core.UpdateInfo()}
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

// ---- approvals ----

type approvalRef struct {
	ApprovalID string `json:"approval_id"`
}

// approvalTicket checks that the link was issued for this very approval.
func approvalTicket(purpose, id string) error {
	if purpose != "approve:"+id {
		return errors.New("this link is for a different request; open the latest link from your agent")
	}
	return nil
}

// challenge binds a user's approval to one instance, one approval, one option, and the exact preview.
func (s *Server) challenge(d approvals.Detail, option string) []byte {
	preview, _ := json.Marshal(d.Preview)
	pd := sha256.Sum256(preview)
	h := sha256.Sum256([]byte(strings.Join([]string{"rubi-approve", "v1", s.core.ID.InstanceID, d.ID, option,
		d.Nonce, hex.EncodeToString(pd[:])}, "|")))
	return h[:]
}

func (s *Server) approvalGet(purpose string, env Envelope) (any, error) {
	var a approvalRef
	if err := json.Unmarshal(env.Args, &a); err != nil {
		return nil, errors.New("bad arguments")
	}
	if err := approvalTicket(purpose, a.ApprovalID); err != nil {
		return nil, err
	}
	if s.core.State() != core.Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	d, err := s.core.Approvals.Detail(a.ApprovalID)
	if err != nil {
		return nil, err
	}
	challenges := map[string]string{}
	for _, o := range d.Options {
		challenges[o.Key] = base64.RawURLEncoding.EncodeToString(s.challenge(d, o.Key))
	}
	var approvers []map[string]string
	hasPassword := false
	_ = s.core.Vault.View(func(v *vault.Data) error {
		for _, ap := range v.Approvers {
			approvers = append(approvers, map[string]string{"credential_id": ap.CredentialID, "label": ap.Label})
		}
		hasPassword = len(v.PasswordApproveKey) > 0
		return nil
	})
	return map[string]any{"approval": d, "challenges": challenges, "approvers": approvers,
		"password": hasPassword, "rp_id": s.rpID(), "agent_notified": s.core.WebhookConfigured()}, nil
}

type decideArgs struct {
	ApprovalID string `json:"approval_id"`
	Option     string `json:"option"`
	Approve    bool   `json:"approve"`
	Proof      struct {
		Type string `json:"type"` // "passkey" | "password"
		webauthn.Assertion
		MAC string `json:"mac,omitempty"` // base64url HMAC-SHA256(password approve key, challenge)
	} `json:"proof"`
}

func (s *Server) approvalDecide(ctx context.Context, purpose string, env Envelope) (any, error) {
	var a decideArgs
	if err := json.Unmarshal(env.Args, &a); err != nil {
		return nil, errors.New("bad arguments")
	}
	if err := approvalTicket(purpose, a.ApprovalID); err != nil {
		return nil, err
	}
	if !a.Approve { // declining needs no proof
		return s.core.Approvals.DecideStrong(ctx, a.ApprovalID, "", false)
	}
	if s.throttled() {
		return nil, errThrottled
	}
	d, err := s.core.Approvals.Detail(a.ApprovalID)
	if err != nil {
		return nil, err
	}
	ch := s.challenge(d, a.Option)
	switch a.Proof.Type {
	case "passkey":
		if err := s.verifyPasskey(a.Proof.Assertion, ch); err != nil {
			return nil, s.fail(err)
		}
	case "password":
		if err := s.verifyPassword(a.Proof.MAC, ch); err != nil {
			return nil, s.fail(err)
		}
	default:
		return nil, errors.New("approval needs a passkey or password proof")
	}
	s.core.Audit.Record("approval.proof_ok", map[string]any{"approval_id": a.ApprovalID, "method": a.Proof.Type})
	return s.core.Approvals.DecideStrong(ctx, a.ApprovalID, a.Option, true)
}

func (s *Server) verifyPasskey(as webauthn.Assertion, challenge []byte) error {
	var found *vault.Approver
	_ = s.core.Vault.View(func(v *vault.Data) error {
		for i := range v.Approvers {
			if v.Approvers[i].CredentialID == as.CredentialID {
				ap := v.Approvers[i]
				found = &ap
			}
		}
		return nil
	})
	if found == nil {
		return errors.New("this passkey is not registered with Rubi")
	}
	count, err := webauthn.Verify(as, webauthn.Expect{Challenge: challenge, Origin: s.core.PanelOrigin,
		RPID: s.rpID(), PublicKey: found.PublicKey, Alg: found.Alg, SignCount: found.SignCount})
	if err != nil {
		return err
	}
	if count != 0 {
		_ = s.core.Vault.Update(func(v *vault.Data) error {
			for i := range v.Approvers {
				if v.Approvers[i].CredentialID == as.CredentialID {
					v.Approvers[i].SignCount = count
				}
			}
			return nil
		})
	}
	return nil
}

func (s *Server) verifyPassword(macB64 string, challenge []byte) error {
	mac, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return errors.New("bad password proof")
	}
	var key []byte
	_ = s.core.Vault.View(func(v *vault.Data) error {
		key = append([]byte(nil), v.PasswordApproveKey...)
		return nil
	})
	if len(key) == 0 {
		return errors.New("password approvals are not set up")
	}
	m := hmac.New(sha256.New, key)
	m.Write(challenge)
	if !hmac.Equal(m.Sum(nil), mac) {
		return errors.New("wrong password")
	}
	return nil
}

func (s *Server) rpID() string {
	u, err := url.Parse(s.core.PanelOrigin)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// ---- settings ----
//
// Settings operations need a "settings" or "setup:<id>" ticket and an unlocked Rubi. Reading is free;
// every change becomes a strong approval. The response carries a fresh ticket for that approval so the
// panel can ask for Face ID (or the password) right away.

func (s *Server) settings(ctx context.Context, purpose string, env Envelope) (any, error) {
	if purpose != "settings" && !strings.HasPrefix(purpose, "setup:") && !strings.HasPrefix(purpose, "agent:") {
		return nil, errors.New("this link can't change settings; ask your agent for a settings link")
	}
	if s.core.State() != core.Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	var args struct {
		ID      string            `json:"id"`
		Plugin  string            `json:"plugin"`
		Fields  map[string]string `json:"fields"`
		Secrets map[string]string `json:"secrets"`
		Levels  map[string]string `json:"levels"`
		URL     string            `json:"url"`
		Key     string            `json:"key"`
		Name    string            `json:"name"`
		Sources []string          `json:"sources"`
		Values  map[string]any    `json:"values"`
		Account string            `json:"account"`
	}
	if len(env.Args) > 0 {
		if err := json.Unmarshal(env.Args, &args); err != nil {
			return nil, errors.New("bad arguments")
		}
	}
	if strings.HasPrefix(purpose, "agent:") {
		// An agent link registers that one Bot's webhook and nothing else.
		name := strings.TrimPrefix(purpose, "agent:")
		switch env.Op {
		case "agents.get":
		case "agent.add", "webhook.test", "agent.subscribe":
			if !strings.EqualFold(strings.TrimSpace(args.Name), strings.TrimSpace(name)) {
				return nil, errors.New("this link is for connecting " + name)
			}
		default:
			return nil, errors.New("this link only connects an agent; ask your agent for a settings link")
		}
	}
	if strings.HasPrefix(purpose, "setup:") {
		// A setup link connects one integration and nothing else.
		switch env.Op {
		case "integration.catalog", "integration.setup", "plugin.config.get", "plugin.config.set":
		default:
			return nil, errors.New("this link only sets up an integration; ask your agent for a settings link")
		}
		if env.Op != "integration.catalog" && args.ID != strings.TrimPrefix(purpose, "setup:") {
			return nil, errors.New("this link is for setting up a different integration")
		}
	}

	var approvalID, account string
	var err error
	switch env.Op {
	case "integration.catalog":
		return s.catalog(), nil
	case "store.list":
		return s.core.StoreList(ctx)
	case "plugin.install", "plugin.update", "plugin.remove", "plugin.rollback":
		var res map[string]any
		switch env.Op {
		case "plugin.install":
			res, err = s.core.RequestPluginInstall(ctx, args.Plugin)
		case "plugin.update":
			res, err = s.core.RequestPluginUpdate(ctx, args.ID)
		case "plugin.rollback":
			res, err = s.core.RequestPluginRollback(ctx, args.ID)
		default:
			res, err = s.core.RequestPluginRemove(ctx, args.ID)
		}
		if err != nil {
			return nil, err
		}
		id, ok := res["approval_id"].(string)
		if !ok {
			return res, nil // e.g. already up to date
		}
		return map[string]any{"approval_id": id, "ticket": s.core.MintTicket("approve:" + id)}, nil
	case "policy.get":
		return s.policy(), nil
	case "webhook.get": // older panels
		agents := s.core.Agents()
		out := map[string]any{"configured": len(agents) > 0}
		for _, a := range agents {
			if a.Default {
				out["url"] = a.Host
			}
		}
		return out, nil
	case "agents.get":
		out := map[string]any{"agents": s.core.Agents()}
		if strings.HasPrefix(purpose, "agent:") {
			out["name"] = strings.TrimPrefix(purpose, "agent:")
		}
		out["sources"] = s.core.Sources()
		return out, nil
	case "agent.subscribe":
		subs, err := s.core.SetSubscriptions(args.Name, args.Sources)
		if err != nil {
			return nil, err
		}
		return map[string]any{"subscriptions": subs}, nil
	case "webhook.test":
		ev := s.core.TestWebhook(args.Name)
		return map[string]any{"event_id": ev.ID}, nil
	case "plugin.config.get":
		return s.core.PluginConfig(args.ID, args.Account)
	case "plugin.config.set":
		approvalID, err = s.core.SetPluginConfig(ctx, args.ID, args.Account, args.Values)
	case "agent.add":
		approvalID, err = s.core.AddAgent(ctx, args.Name, args.URL, args.Key, args.Sources)
	case "agent.remove":
		approvalID, err = s.core.RemoveAgent(ctx, args.Name)
	case "agent.default":
		approvalID, err = s.core.SetDefaultAgent(ctx, args.Name)

	case "integration.setup":
		approvalID, account, err = s.core.ConnectIntegration(ctx, args.ID, args.Fields, args.Secrets)
	case "integration.disconnect":
		approvalID, err = s.core.DisconnectIntegration(ctx, args.ID, args.Account)
	case "policy.set":
		approvalID, err = s.core.SetPolicy(ctx, args.Levels)
	case "webhook.set":
		approvalID, err = s.core.SetWebhook(ctx, args.URL, args.Key)
	}
	var more *rubiplugin.NeedMore
	if errors.As(err, &more) {
		return map[string]any{"need_more": more}, nil
	}
	if err != nil {
		return nil, err
	}
	res := map[string]any{"approval_id": approvalID, "ticket": s.core.MintTicket("approve:" + approvalID)}
	if account != "" {
		res["account"] = account
	}
	return res, nil
}

func (s *Server) updates(ctx context.Context, env Envelope) (any, error) {
	if s.core.State() != core.Unlocked {
		return nil, errors.New("Rubi is locked; unlock it first")
	}
	var args struct {
		ID     string `json:"id"`
		Notify bool   `json:"notify"`
	}
	if len(env.Args) > 0 {
		if err := json.Unmarshal(env.Args, &args); err != nil {
			return nil, errors.New("bad arguments")
		}
	}
	var res map[string]any
	var err error
	switch env.Op {
	case "updates.list":
		return map[string]any{"updates": s.core.Updates(ctx)}, nil
	case "updates.notify":
		if err := s.core.SetUpdateNotify(args.ID, args.Notify); err != nil {
			return nil, err
		}
		return map[string]any{"id": args.ID, "notify": args.Notify}, nil
	case "updates.rubi":
		res, err = s.core.RequestUpdate(ctx)
	default:
		res, err = s.core.RequestPluginUpdate(ctx, args.ID)
	}
	if err != nil {
		return nil, err
	}
	id, ok := res["approval_id"].(string)
	if !ok {
		return res, nil
	}
	return map[string]any{"approval_id": id, "ticket": s.core.MintTicket("approve:" + id)}, nil
}

func (s *Server) catalog() any {
	accounts := map[string][]string{}
	_ = s.core.Vault.View(func(d *vault.Data) error {
		for id, i := range d.Integrations {
			for _, a := range i.Accounts {
				accounts[id] = append(accounts[id], a.Label)
			}
		}
		return nil
	})
	var list []map[string]any
	for _, m := range s.core.Store.Installed() {
		entry := map[string]any{"id": m.ID, "name": m.Name, "description": m.Description, "needs": m.Needs,
			"fields": m.Fields, "secrets": m.Secrets, "egress": m.Egress, "connected": false, "has_config": len(m.Config) > 0}
		if a := accounts[m.ID]; len(a) > 0 {
			entry["connected"], entry["accounts"] = true, a
		}
		list = append(list, entry)
	}
	return map[string]any{"integrations": list}
}

func (s *Server) policy() any {
	var actions []map[string]any
	for _, m := range s.core.Store.Installed() {
		for _, a := range m.Actions {
			actions = append(actions, map[string]any{"integration": m.Name, "kind": a.Kind, "title": a.Title,
				"default": a.DefaultLevel, "level": s.core.PolicyLevel(a.Kind), "locked": a.Locked})
		}
	}
	return map[string]any{"actions": actions, "levels": []approvals.Level{approvals.None, approvals.Chat, approvals.Strong}}
}
