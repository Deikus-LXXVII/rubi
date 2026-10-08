// Package panelclient is a Go implementation of the panel's side of the protocol.
//
// The real panel is a static web app (M3) that does the same with WebCrypto. This package is the reference
// for that implementation, the client used in tests, and the development client (`rubi dev-panel`).
//
// Key wrapping (performed only on the user's device):
//
//	password KEK  = Argon2id(password, salt[16], time=3, memory=64 MiB, threads=1, 32 bytes)
//	wrap          = AES-256-GCM(KEK, nonce[12], DEK, ad="rubi-wrap|v1|" + instance + "|" + wrap_id)
//	approve key   = HKDF-SHA256(ikm=password KEK, info="rubi approve v1", 32 bytes)
//	password proof = HMAC-SHA256(approve key, approval challenge)
package panelclient

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"net/http"
	"net/url"
	"strings"
	"time"

	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/Deikus-LXXVII/rubi/internal/webauthn"
)

var b64 = base64.RawURLEncoding

type Link struct {
	Endpoint string   // HTTP transport (tunnel or fixed URL), if any
	Relay    string   // Rubi's routing key on the relays, if any
	Relays   []string // relays to use (default list when empty)
	Instance string   // public bundle
	Purpose  string
	Pairing  string
	Ticket   string
}

// ParseLink reads a panel link (https://rubi-panel.com/#v=1&e=…&k=…&a=…&p=…|t=…).
func ParseLink(s string) (Link, error) {
	i := strings.Index(s, "#")
	if i < 0 {
		return Link{}, errors.New("not a Rubi panel link")
	}
	q, err := url.ParseQuery(s[i+1:])
	if err != nil || q.Get("v") != "1" || (q.Get("e") == "" && q.Get("n") == "") || q.Get("k") == "" {
		return Link{}, errors.New("not a Rubi panel link")
	}
	l := Link{Endpoint: strings.TrimRight(q.Get("e"), "/"), Relay: q.Get("n"), Instance: q.Get("k"), Purpose: q.Get("a"),
		Pairing: q.Get("p"), Ticket: q.Get("t")}
	if r := q.Get("r"); r != "" {
		l.Relays = strings.Split(r, ",")
	}
	return l, nil
}

type Client struct {
	Link Link
	HTTP *http.Client
	// PreferHTTP uses the HTTP endpoint even when the link also offers relays.
	PreferHTTP bool
	ch         e2e.Client
	relay      *relay.Client
}

// post sends a sealed request over the link's transport: relays when offered, else HTTP.
func (c *Client) post(body []byte) ([]byte, error) {
	if c.Link.Relay != "" && (c.Link.Endpoint == "" || !c.PreferHTTP) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c.relay == nil {
			relays := c.Link.Relays
			if len(relays) == 0 {
				relays = relay.DefaultRelays
			}
			rc, err := relay.Dial(ctx, c.Link.Relay, relays)
			if err != nil {
				return nil, err
			}
			c.relay = rc
		}
		status, resp, err := c.relay.Do(ctx, body)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("panel API returned %d", status)
		}
		return resp, nil
	}
	resp, err := c.HTTP.Post(c.Link.Endpoint+panelapi.RPCPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("panel API returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func New(l Link) (*Client, error) {
	pub, err := e2e.ParseInstanceBundle(l.Instance)
	if err != nil {
		return nil, err
	}
	return &Client{Link: l, HTTP: &http.Client{Timeout: 30 * time.Second}, ch: e2e.Client{InstanceKey: pub}}, nil
}

// Call performs one encrypted RPC. A response that doesn't decrypt means the endpoint isn't the pinned
// instance (e.g. a hijacked tunnel), and is reported as such.
func (c *Client) Call(op string, args any, out any) error {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return err
		}
		raw = b
	}
	ridBytes := make([]byte, 16)
	_, _ = rand.Read(ridBytes)
	env := panelapi.Envelope{Op: op, TS: time.Now().Unix(), RID: b64.EncodeToString(ridBytes), Ticket: c.Link.Ticket, Args: raw}
	plain, _ := json.Marshal(env)
	if n := panelapi.PadTarget(len(plain)+len(`,"pad":""`)) - len(plain) - len(`,"pad":""`); n > 0 {
		env.Pad = strings.Repeat("0", n)
		plain, _ = json.Marshal(env)
	}
	ch := c.ch
	if panelapi.Sensitive(op) {
		// Keys and passwords go sealed to this Rubi process's session key as well (forward secrecy);
		// it is fetched fresh, since it changes whenever Rubi restarts.
		var hello struct {
			SessionKey string `json:"session_key"`
		}
		if err := c.Call("hello", nil, &hello); err != nil {
			return err
		}
		skb, err := b64.DecodeString(hello.SessionKey)
		if err != nil {
			return errors.New("this Rubi offers no session key")
		}
		sk, err := ecdh.X25519().NewPublicKey(skb)
		if err != nil {
			return errors.New("this Rubi offers no session key")
		}
		ch.SessionKey = sk
	}
	req, respKey, err := ch.Seal(panelapi.RPCPath, plain)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(req)
	respBody, err := c.post(body)
	if err != nil {
		return err
	}
	var sealed e2e.Response
	if err := json.Unmarshal(respBody, &sealed); err != nil {
		return err
	}
	opened, err := e2e.OpenResponse(respKey, env.RID, sealed)
	if err != nil {
		return errors.New("the response did not come from the pinned Rubi instance")
	}
	var r struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
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

type Hello struct {
	Instance    string `json:"instance"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Version     string `json:"version"`
}

type PasswordKDF struct {
	Alg       string `json:"alg"`
	Salt      string `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

func DefaultKDF() PasswordKDF {
	s := make([]byte, 16)
	_, _ = rand.Read(s)
	return PasswordKDF{Alg: "argon2id", Salt: b64.EncodeToString(s), Time: 3, MemoryKiB: 64 * 1024, Threads: 1}
}

func (k PasswordKDF) derive(password string) ([]byte, error) {
	if k.Alg != "argon2id" || k.Time < 2 || k.Time > 10 || k.MemoryKiB < 46*1024 || k.MemoryKiB > 1024*1024 || k.Threads < 1 || k.Threads > 4 {
		return nil, errors.New("unsupported password KDF parameters")
	}
	salt, err := b64.DecodeString(k.Salt)
	if err != nil || len(salt) < 16 {
		return nil, errors.New("bad KDF salt")
	}
	return argon2.IDKey([]byte(password), salt, k.Time, k.MemoryKiB, k.Threads, 32), nil
}

func wrapAD(instance, wrapID string) []byte { return []byte("rubi-wrap|v1|" + instance + "|" + wrapID) }

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// ApproveKey derives the password approval key from the password KEK.
func ApproveKey(kek []byte) []byte {
	k := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, kek, nil, []byte("rubi approve v1")), k); err != nil {
		panic(err)
	}
	return k
}

// PasswordWrap wraps dek under a key derived from password. It also returns the password KEK.
func PasswordWrap(instance, password string, dek []byte) (vault.Wrap, []byte, error) {
	kdf := DefaultKDF()
	kek, err := kdf.derive(password)
	if err != nil {
		return vault.Wrap{}, nil, err
	}
	aead, err := gcm(kek)
	if err != nil {
		return vault.Wrap{}, nil, err
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	id := "w_" + b64.EncodeToString(idb)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	kdfJSON, _ := json.Marshal(kdf)
	return vault.Wrap{ID: id, Kind: "password", KDF: kdfJSON, Label: "Password",
		Nonce: b64.EncodeToString(nonce), Wrapped: b64.EncodeToString(aead.Seal(nil, nonce, dek, wrapAD(instance, id))),
		CreatedAt: time.Now().UTC()}, kek, nil
}

// UnwrapPassword recovers the DEK from a password wrap.
func UnwrapPassword(instance string, w vault.Wrap, password string) ([]byte, error) {
	dek, _, err := unwrapPassword(instance, w, password)
	return dek, err
}

func unwrapPassword(instance string, w vault.Wrap, password string) (dek, kek []byte, err error) {
	var kdf PasswordKDF
	if err := json.Unmarshal(w.KDF, &kdf); err != nil {
		return nil, nil, err
	}
	kek, err = kdf.derive(password)
	if err != nil {
		return nil, nil, err
	}
	aead, err := gcm(kek)
	if err != nil {
		return nil, nil, err
	}
	nonce, err1 := b64.DecodeString(w.Nonce)
	ct, err2 := b64.DecodeString(w.Wrapped)
	if err1 != nil || err2 != nil {
		return nil, nil, errors.New("corrupted key wrap")
	}
	dek, err = aead.Open(nil, nonce, ct, wrapAD(instance, w.ID))
	if err != nil {
		return nil, nil, errors.New("wrong password")
	}
	return dek, kek, nil
}

// PairWithPassword creates the vault key on this device and pairs Rubi, optionally registering passkey
// approvers. Returns the vault version.
func (c *Client) PairWithPassword(password string, approvers ...vault.Approver) (uint64, error) {
	if c.Link.Pairing == "" {
		return 0, errors.New("this is not a pairing link")
	}
	var h Hello
	if err := c.Call("hello", nil, &h); err != nil {
		return 0, err
	}
	dek := vault.NewKey()
	w, kek, err := PasswordWrap(h.Instance, password, dek)
	if err != nil {
		return 0, err
	}
	var res struct {
		VaultVersion uint64 `json:"vault_version"`
	}
	err = c.Call("pair", map[string]any{"code": c.Link.Pairing, "dek": b64.EncodeToString(dek),
		"wraps": []vault.Wrap{w}, "approvers": approvers,
		"password_approve_key": b64.EncodeToString(ApproveKey(kek)), "method": "password"}, &res)
	return res.VaultVersion, err
}

// ApprovalInfo is what the panel shows before the user decides.
type ApprovalInfo struct {
	Approval struct {
		ID       string           `json:"approval_id"`
		Kind     string           `json:"kind"`
		Summary  string           `json:"summary"`
		State    string           `json:"state"`
		Question string           `json:"question"`
		Preview  json.RawMessage  `json:"preview"`
		Options  []approvalOption `json:"options"`
	} `json:"approval"`
	Challenges map[string]string `json:"challenges"`
	Password   bool              `json:"password"`
	RPID       string            `json:"rp_id"`
}

type approvalOption struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

func (c *Client) ApprovalInfo(id string) (*ApprovalInfo, error) {
	var info ApprovalInfo
	err := c.Call("approval.get", map[string]string{"approval_id": id}, &info)
	return &info, err
}

// ApproveWithPassword proves knowledge of the password for one approval and option.
func (c *Client) ApproveWithPassword(id, option, password string) (map[string]any, error) {
	info, err := c.ApprovalInfo(id)
	if err != nil {
		return nil, err
	}
	chB64 := info.Challenges[option]
	if chB64 == "" && strings.HasPrefix(option, "items:") { // a chosen subset of a batch
		var r struct {
			Challenge string `json:"challenge"`
		}
		if err := c.Call("approval.challenge", map[string]string{"approval_id": id, "option": option}, &r); err != nil {
			return nil, err
		}
		chB64 = r.Challenge
	}
	ch, err := b64.DecodeString(chB64)
	if err != nil || len(ch) == 0 {
		return nil, fmt.Errorf("unknown option %q", option)
	}
	var keys struct {
		Instance string       `json:"instance"`
		Wraps    []vault.Wrap `json:"wraps"`
	}
	if err := c.Call("unlock.keys", nil, &keys); err != nil {
		return nil, err
	}
	for _, w := range keys.Wraps {
		if w.Kind != "password" {
			continue
		}
		_, kek, err := unwrapPassword(keys.Instance, w, password)
		if err != nil {
			continue
		}
		m := hmac.New(sha256.New, ApproveKey(kek))
		m.Write(ch)
		return c.decide(id, option, map[string]any{"type": "password", "mac": b64.EncodeToString(m.Sum(nil))})
	}
	return nil, errors.New("wrong password")
}

// ApproveWithPasskey sends a WebAuthn assertion produced by sign over the option's challenge.
func (c *Client) ApproveWithPasskey(id, option string, sign func(challenge []byte) webauthn.Assertion) (map[string]any, error) {
	info, err := c.ApprovalInfo(id)
	if err != nil {
		return nil, err
	}
	ch, err := b64.DecodeString(info.Challenges[option])
	if err != nil || len(ch) == 0 {
		return nil, fmt.Errorf("unknown option %q", option)
	}
	a := sign(ch)
	return c.decide(id, option, map[string]any{"type": "passkey", "credential_id": a.CredentialID,
		"client_data_json": a.ClientDataJSON, "authenticator_data": a.AuthenticatorData, "signature": a.Signature})
}

func (c *Client) Deny(id string) (map[string]any, error) {
	var out map[string]any
	err := c.Call("approval.decide", map[string]any{"approval_id": id, "approve": false}, &out)
	return out, err
}

func (c *Client) decide(id, option string, proof map[string]any) (map[string]any, error) {
	var out map[string]any
	err := c.Call("approval.decide", map[string]any{"approval_id": id, "option": option, "approve": true,
		"proof": proof}, &out)
	return out, err
}

// UnlockWithPassword unwraps the DEK locally and sends only the DEK. Returns the vault version.
func (c *Client) UnlockWithPassword(password string, minVersion uint64) (uint64, error) {
	var keys struct {
		Instance string       `json:"instance"`
		Wraps    []vault.Wrap `json:"wraps"`
	}
	if err := c.Call("unlock.keys", nil, &keys); err != nil {
		return 0, err
	}
	var dek []byte
	var lastErr error = errors.New("no password is set up for this Rubi")
	for _, w := range keys.Wraps {
		if w.Kind != "password" {
			continue
		}
		if dek, lastErr = UnwrapPassword(keys.Instance, w, password); lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		dek = nil
	}
	if dek == nil {
		return 0, lastErr
	}
	var res struct {
		VaultVersion uint64 `json:"vault_version"`
	}
	err := c.Call("unlock", map[string]any{"dek": b64.EncodeToString(dek), "min_vault_version": minVersion,
		"method": "password"}, &res)
	return res.VaultVersion, err
}
