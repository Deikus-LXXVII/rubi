// Package identity holds the instance's long-term key pairs.
//
// The panel pins the public half on first pairing and uses it to encrypt to Rubi and to verify Rubi's
// responses. The private half is stored unencrypted (Rubi needs it before unlock); see the security model
// for why that is acceptable.
package identity

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

type Identity struct {
	InstanceID string
	Box        *ecdh.PrivateKey   // X25519, for the end-to-end channel
	Sign       ed25519.PrivateKey // Ed25519, for authenticating responses
	CreatedAt  time.Time
}

type stored struct {
	InstanceID string    `json:"instance_id"`
	X25519     string    `json:"x25519_private"`
	Ed25519    string    `json:"ed25519_seed"`
	CreatedAt  time.Time `json:"created_at"`
}

var b64 = base64.RawURLEncoding

// LoadOrCreate reads the identity at path, creating a new one on first run.
func LoadOrCreate(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return create(path)
	}
	if err != nil {
		return nil, err
	}
	var s stored
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	xb, err := b64.DecodeString(s.X25519)
	if err != nil {
		return nil, err
	}
	box, err := ecdh.X25519().NewPrivateKey(xb)
	if err != nil {
		return nil, err
	}
	seed, err := b64.DecodeString(s.Ed25519)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("identity: bad ed25519 seed")
	}
	return &Identity{InstanceID: s.InstanceID, Box: box, Sign: ed25519.NewKeyFromSeed(seed), CreatedAt: s.CreatedAt}, nil
}

func create(path string) (*Identity, error) {
	box, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	idBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	id := &Identity{InstanceID: "rubi_" + b64.EncodeToString(idBytes), Box: box, Sign: sign, CreatedAt: time.Now().UTC()}
	b, err := json.MarshalIndent(stored{
		InstanceID: id.InstanceID,
		X25519:     b64.EncodeToString(box.Bytes()),
		Ed25519:    b64.EncodeToString(sign.Seed()),
		CreatedAt:  id.CreatedAt,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := vault.WriteFileAtomic(path, b, 0o600); err != nil {
		return nil, err
	}
	return id, nil
}

// PublicBundle is what the panel pins: X25519 public key || Ed25519 public key, base64url.
func (i *Identity) PublicBundle() string {
	pub := append(append([]byte{}, i.Box.PublicKey().Bytes()...), i.Sign.Public().(ed25519.PublicKey)...)
	return b64.EncodeToString(pub)
}

// Fingerprint is a short, human-comparable digest of the public bundle.
func (i *Identity) Fingerprint() string {
	sum := sha256.Sum256([]byte(i.PublicBundle()))
	return b64.EncodeToString(sum[:9])
}
