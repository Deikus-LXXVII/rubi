// Package panelclient is a Go implementation of the panel's side of the protocol.
//
// The real panel is a static web app (M3) that does the same with WebCrypto. This package is the reference
// for that implementation, the client used in tests, and the development client (`rubi dev-panel`).
//
// Key wrapping (performed only on the user's device):
//
//	password KEK = Argon2id(password, salt[16], time=3, memory=64 MiB, threads=1, 32 bytes)
//	wrap         = AES-256-GCM(KEK, nonce[12], DEK, ad="rubi-wrap|v1|" + instance + "|" + wrap_id)
package panelclient

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

var b64 = base64.RawURLEncoding

type Link struct {
	Endpoint string
	Instance string // public bundle
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
	if err != nil || q.Get("v") != "1" || q.Get("e") == "" || q.Get("k") == "" {
		return Link{}, errors.New("not a Rubi panel link")
	}
	return Link{Endpoint: strings.TrimRight(q.Get("e"), "/"), Instance: q.Get("k"), Purpose: q.Get("a"),
		Pairing: q.Get("p"), Ticket: q.Get("t")}, nil
}

type Client struct {
	Link Link
	HTTP *http.Client
	ch   e2e.Client
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
	req, respKey, err := c.ch.Seal(panelapi.RPCPath, plain)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(req)
	resp, err := c.HTTP.Post(c.Link.Endpoint+panelapi.RPCPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel API returned HTTP %d", resp.StatusCode)
	}
	var sealed e2e.Response
	if err := json.NewDecoder(resp.Body).Decode(&sealed); err != nil {
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
	if k.Alg != "argon2id" || k.Time == 0 || k.MemoryKiB < 19*1024 || k.Threads == 0 {
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

// PasswordWrap wraps dek under a key derived from password.
func PasswordWrap(instance, password string, dek []byte) (vault.Wrap, error) {
	kdf := DefaultKDF()
	kek, err := kdf.derive(password)
	if err != nil {
		return vault.Wrap{}, err
	}
	aead, err := gcm(kek)
	if err != nil {
		return vault.Wrap{}, err
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	id := "w_" + b64.EncodeToString(idb)
	nonce := make([]byte, aead.NonceSize())
	_, _ = rand.Read(nonce)
	kdfJSON, _ := json.Marshal(kdf)
	return vault.Wrap{ID: id, Kind: "password", KDF: kdfJSON, Label: "Password",
		Nonce: b64.EncodeToString(nonce), Wrapped: b64.EncodeToString(aead.Seal(nil, nonce, dek, wrapAD(instance, id))),
		CreatedAt: time.Now().UTC()}, nil
}

// UnwrapPassword recovers the DEK from a password wrap.
func UnwrapPassword(instance string, w vault.Wrap, password string) ([]byte, error) {
	var kdf PasswordKDF
	if err := json.Unmarshal(w.KDF, &kdf); err != nil {
		return nil, err
	}
	kek, err := kdf.derive(password)
	if err != nil {
		return nil, err
	}
	aead, err := gcm(kek)
	if err != nil {
		return nil, err
	}
	nonce, err1 := b64.DecodeString(w.Nonce)
	ct, err2 := b64.DecodeString(w.Wrapped)
	if err1 != nil || err2 != nil {
		return nil, errors.New("corrupted key wrap")
	}
	dek, err := aead.Open(nil, nonce, ct, wrapAD(instance, w.ID))
	if err != nil {
		return nil, errors.New("wrong password")
	}
	return dek, nil
}

// PairWithPassword creates the vault key on this device and pairs Rubi. Returns the vault version.
func (c *Client) PairWithPassword(password string) (uint64, error) {
	if c.Link.Pairing == "" {
		return 0, errors.New("this is not a pairing link")
	}
	var h Hello
	if err := c.Call("hello", nil, &h); err != nil {
		return 0, err
	}
	dek := vault.NewKey()
	w, err := PasswordWrap(h.Instance, password, dek)
	if err != nil {
		return 0, err
	}
	var res struct {
		VaultVersion uint64 `json:"vault_version"`
	}
	err = c.Call("pair", map[string]any{"code": c.Link.Pairing, "dek": b64.EncodeToString(dek),
		"wraps": []vault.Wrap{w}, "method": "password"}, &res)
	return res.VaultVersion, err
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
