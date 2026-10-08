// Package relay carries the panel protocol over public Nostr relays.
//
// Agent machines have no inbound address, and account-less tunnels are rate-limited per IP address, which
// many agent machines share. Relays need neither: Rubi and the panel both connect out to the same public
// relays and exchange ephemeral events (kind 20000-29999, never stored). Each event carries a chunk of an
// already end-to-end encrypted panel request or response, so relays only see ciphertext and timing.
// Rubi uses several relays at once; any one of them is enough.
//
// The Nostr keys here are throwaway routing identities. Authenticity comes from the panel protocol's own
// encryption, which binds every exchange to Rubi's instance key from the link.
package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Kind is the ephemeral event kind Rubi uses.
const Kind = 21777

// HookKind carries a web request that Rubi Gateway received at a hook address (see hooks.go). Only the
// gateway publishes these; clients can't.
const HookKind = 21779

type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// Key is a Nostr signing key.
type Key struct {
	priv *btcec.PrivateKey
}

func NewKey() (*Key, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	priv, _ := btcec.PrivKeyFromBytes(b)
	return &Key{priv: priv}, nil
}

// KeyFromHex restores a key saved with Hex.
func KeyFromHex(s string) (*Key, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("bad relay key")
	}
	priv, _ := btcec.PrivKeyFromBytes(b)
	return &Key{priv: priv}, nil
}

func (k *Key) Hex() string { return hex.EncodeToString(k.priv.Serialize()) }

// Public is the x-only public key in hex, as Nostr uses it.
func (k *Key) Public() string { return hex.EncodeToString(schnorr.SerializePubKey(k.priv.PubKey())) }

// serialize is NIP-01's canonical form. Go's default JSON escaping of <, > and & would give ids that
// relays and other clients compute differently, so HTML escaping is off.
func serialize(e *Event) ([]byte, error) {
	tags := e.Tags
	if tags == nil {
		tags = [][]string{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode([]any{0, e.PubKey, e.CreatedAt, e.Kind, tags, e.Content}); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Sign fills in pubkey, id and signature.
func (k *Key) Sign(e *Event) error {
	e.PubKey = k.Public()
	if e.Tags == nil {
		e.Tags = [][]string{}
	}
	ser, err := serialize(e)
	if err != nil {
		return err
	}
	id := sha256.Sum256(ser)
	sig, err := schnorr.Sign(k.priv, id[:])
	if err != nil {
		return err
	}
	e.ID, e.Sig = hex.EncodeToString(id[:]), hex.EncodeToString(sig.Serialize())
	return nil
}

// Verify checks an event's id and signature.
func Verify(e *Event) error {
	ser, err := serialize(e)
	if err != nil {
		return err
	}
	id := sha256.Sum256(ser)
	if hex.EncodeToString(id[:]) != e.ID {
		return errors.New("event id mismatch")
	}
	pkb, err1 := hex.DecodeString(e.PubKey)
	sigb, err2 := hex.DecodeString(e.Sig)
	if err := errors.Join(err1, err2); err != nil {
		return err
	}
	pk, err := schnorr.ParsePubKey(pkb)
	if err != nil {
		return err
	}
	sig, err := schnorr.ParseSignature(sigb)
	if err != nil {
		return err
	}
	if !sig.Verify(id[:], pk) {
		return fmt.Errorf("bad signature")
	}
	return nil
}

// fresh rejects events far from now (relays may replay or hold events).
func fresh(e *Event, now time.Time) bool {
	t := time.Unix(e.CreatedAt, 0)
	return t.After(now.Add(-2*time.Minute)) && t.Before(now.Add(2*time.Minute))
}
