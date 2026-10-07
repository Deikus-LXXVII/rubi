package vault

import (
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Wrap is one copy of the DEK encrypted by a key-encryption key that only the user's device can derive.
// Rubi stores wraps verbatim and hands them to the panel; it can't open them itself.
type Wrap struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // "passkey" | "password"
	// CredentialID identifies the passkey (base64url) for kind "passkey".
	CredentialID string `json:"credential_id,omitempty"`
	// KDF parameters for kind "password" (e.g. Argon2id salt and cost), opaque to Rubi.
	KDF json.RawMessage `json:"kdf,omitempty"`
	// PRF salt for kind "passkey", opaque to Rubi.
	PRFSalt   string    `json:"prf_salt,omitempty"`
	Nonce     string    `json:"nonce"`
	Wrapped   string    `json:"wrapped"`
	Label     string    `json:"label,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Keys struct {
	Instance string `json:"instance"`
	Wraps    []Wrap `json:"wraps"`
}

func LoadKeys(path string) (*Keys, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var k Keys
	if err := json.Unmarshal(b, &k); err != nil {
		return nil, err
	}
	return &k, nil
}

func SaveKeys(path string, k *Keys) error {
	if len(k.Wraps) == 0 {
		return errors.New("refusing to save keys without any wrap: the vault would become unrecoverable")
	}
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b, 0o600)
}
