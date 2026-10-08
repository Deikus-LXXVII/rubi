// Package vault stores all of Rubi's private state encrypted under a 256-bit data key (DEK).
//
// The DEK itself is never stored in plaintext. It is wrapped in the user's browser by key-encryption
// keys derived from a passkey (WebAuthn PRF) or a password, and those wraps are kept in keys.json.
// Rubi only ever sees the DEK, delivered by the panel on unlock, and holds it in memory while unlocked.
//
// The vault is sealed with XChaCha20-Poly1305. The associated data binds the ciphertext to the format,
// the instance id and the vault version, so a vault can't be swapped between instances, and its header
// can't be edited without detection.
package vault

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const Format = "rubi-vault/1"

// KeySize is the DEK length in bytes.
const KeySize = chacha20poly1305.KeySize

var (
	ErrLocked  = errors.New("vault is locked")
	ErrBadKey  = errors.New("wrong key or corrupted vault")
	ErrNoVault = errors.New("vault does not exist")
	ErrVersion = errors.New("vault version mismatch (possible rollback or tampering)")
)

// Data is everything Rubi keeps private. It is only ever in memory while unlocked.
type Data struct {
	Version  uint64 `json:"version"`
	Instance string `json:"instance"`
	// Policy maps an action kind (e.g. "icloud-mail.send") to its approval level.
	Policy map[string]string `json:"policy"`
	// Integrations holds per-integration settings and secrets, namespaced by integration id.
	Integrations map[string]*Integration `json:"integrations"`
	// Webhook is the single agent webhook of v0.3.1 and earlier, migrated into Agents on load.
	Webhook *Webhook `json:"webhook,omitempty"`
	// Agents are the user's Grok Bots that Rubi can wake, each through its own routine webhook.
	Agents []*Agent `json:"agents,omitempty"`
	Locale string   `json:"locale,omitempty"`
	// Approvers are public keys of the user's passkeys, used to verify action approvals.
	Approvers []Approver `json:"approvers,omitempty"`
	// PasswordApproveKey verifies password-based approvals (HMAC). It is derived on the user's device
	// from the password; weaker than a passkey because Rubi has to hold it.
	PasswordApproveKey []byte `json:"password_approve_key,omitempty"`
	// Receipts record recent unlocks so the user can spot unexpected ones (newest last, capped).
	Receipts []Receipt `json:"receipts,omitempty"`
	// UpdateNotified is the last release the agent was told about, so each one is announced once.
	UpdateNotified string `json:"update_notified,omitempty"`
	// Plugins records what the user approved installing. A plugin only starts if its files still match.
	Plugins map[string]*Plugin `json:"plugins,omitempty"`
}

// Plugin is the trusted record of an installed plugin.
type Plugin struct {
	Version       string    `json:"version"`
	Source        string    `json:"source"`
	Reviewed      bool      `json:"reviewed"`
	PublisherName string    `json:"publisher_name"`
	PublisherKey  string    `json:"publisher_key"`
	SumsSHA256    string    `json:"sums_sha256"`
	Tree          string    `json:"tree"`
	InstalledAt   time.Time `json:"installed_at"`
	// Notified is the last update the agent was told about.
	Notified string `json:"notified,omitempty"`
	// Previous is the version this one replaced, kept on disk so the user can roll back to it.
	Previous *PluginVersion `json:"previous,omitempty"`
}

// PluginVersion is what's needed to trust and start one installed version of a plugin.
type PluginVersion struct {
	Version       string `json:"version"`
	Source        string `json:"source"`
	Reviewed      bool   `json:"reviewed"`
	PublisherName string `json:"publisher_name"`
	PublisherKey  string `json:"publisher_key"`
	SumsSHA256    string `json:"sums_sha256"`
	Tree          string `json:"tree"`
}

// Current returns the trusted facts of the installed version.
func (p *Plugin) Current() PluginVersion {
	return PluginVersion{Version: p.Version, Source: p.Source, Reviewed: p.Reviewed, PublisherName: p.PublisherName,
		PublisherKey: p.PublisherKey, SumsSHA256: p.SumsSHA256, Tree: p.Tree}
}

// Use makes v the installed version.
func (p *Plugin) Use(v PluginVersion) {
	p.Version, p.Source, p.Reviewed, p.PublisherName = v.Version, v.Source, v.Reviewed, v.PublisherName
	p.PublisherKey, p.SumsSHA256, p.Tree = v.PublisherKey, v.SumsSHA256, v.Tree
}

type Receipt struct {
	At           time.Time `json:"at"`
	Event        string    `json:"event"`  // "paired" | "unlocked"
	Method       string    `json:"method"` // "passkey" | "password"
	CredentialID string    `json:"credential_id,omitempty"`
}

// MaxReceipts bounds the receipt history kept in the vault.
const MaxReceipts = 50

type Integration struct {
	Enabled  bool              `json:"enabled"`
	Account  string            `json:"account,omitempty"` // e.g. the connected email address
	Settings json.RawMessage   `json:"settings,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
	// State is integration-private runtime state (e.g. tracked messages), kept encrypted with the rest.
	State json.RawMessage `json:"state,omitempty"`
	// Config holds user-only settings (manifest config) the user changed from their defaults.
	Config map[string]json.RawMessage `json:"config,omitempty"`
}

// Agent is one Bot Rubi can wake. Events for it go to its routine webhook.
type Agent struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Key     string `json:"key"`
	Default bool   `json:"default,omitempty"` // gets events nobody subscribed to
	// Subscriptions are the event sources this agent hears about: plugin ids, and "rubi" for Rubi's own
	// events (updates, plugin problems).
	Subscriptions []string `json:"subscriptions,omitempty"`
}

type Webhook struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

type Approver struct {
	CredentialID string `json:"credential_id"` // base64url
	PublicKey    []byte `json:"public_key"`    // SubjectPublicKeyInfo DER
	Alg          int    `json:"alg"`           // COSE algorithm (-7 ES256, -8 EdDSA, -257 RS256)
	SignCount    uint32 `json:"sign_count"`
	Label        string `json:"label"`
}

// normalize makes sure every map is usable (empty maps don't survive a JSON round trip with omitempty).
func (d *Data) normalize() {
	if d.Policy == nil {
		d.Policy = map[string]string{}
	}
	if d.Integrations == nil {
		d.Integrations = map[string]*Integration{}
	}
	if d.Plugins == nil {
		d.Plugins = map[string]*Plugin{}
	}
	if d.Webhook != nil && len(d.Agents) == 0 && d.Webhook.URL != "" {
		d.Agents = []*Agent{{Name: "Main", URL: d.Webhook.URL, Key: d.Webhook.Key, Default: true}}
	}
	d.Webhook = nil
}

func NewData(instance string) *Data {
	return &Data{Instance: instance, Policy: map[string]string{}, Integrations: map[string]*Integration{},
		Plugins: map[string]*Plugin{}}
}

type envelope struct {
	Format   string `json:"format"`
	Instance string `json:"instance"`
	Version  uint64 `json:"version"`
	Nonce    string `json:"nonce"`
	Sealed   string `json:"sealed"`
}

func associatedData(instance string, version uint64) []byte {
	return []byte(Format + "|" + instance + "|" + strconv.FormatUint(version, 10))
}

// Seal encrypts d under dek. The envelope version is taken from d.Version.
func Seal(dek []byte, d *Data) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(dek)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := aead.Seal(nil, nonce, plain, associatedData(d.Instance, d.Version))
	return json.MarshalIndent(envelope{
		Format: Format, Instance: d.Instance, Version: d.Version,
		Nonce:  base64.StdEncoding.EncodeToString(nonce),
		Sealed: base64.StdEncoding.EncodeToString(sealed),
	}, "", "  ")
}

// Open decrypts a sealed vault. expectInstance must match the instance the vault was sealed for.
func Open(dek []byte, expectInstance string, blob []byte) (*Data, error) {
	var env envelope
	if err := json.Unmarshal(blob, &env); err != nil {
		return nil, fmt.Errorf("vault envelope: %w", err)
	}
	if env.Format != Format {
		return nil, fmt.Errorf("unsupported vault format %q", env.Format)
	}
	if env.Instance != expectInstance {
		return nil, ErrBadKey
	}
	aead, err := chacha20poly1305.NewX(dek)
	if err != nil {
		return nil, err
	}
	nonce, err1 := base64.StdEncoding.DecodeString(env.Nonce)
	sealed, err2 := base64.StdEncoding.DecodeString(env.Sealed)
	if err1 != nil || err2 != nil || len(nonce) != aead.NonceSize() {
		return nil, ErrBadKey
	}
	plain, err := aead.Open(nil, nonce, sealed, associatedData(env.Instance, env.Version))
	if err != nil {
		return nil, ErrBadKey
	}
	var d Data
	if err := json.Unmarshal(plain, &d); err != nil {
		return nil, fmt.Errorf("vault payload: %w", err)
	}
	if d.Version != env.Version || d.Instance != env.Instance {
		return nil, ErrVersion
	}
	d.normalize()
	return &d, nil
}

// NewKey returns a fresh random DEK.
func NewKey() []byte {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return k
}

// Store is the on-disk vault plus its in-memory unlocked state.
type Store struct {
	path     string
	instance string

	mu   sync.RWMutex
	dek  []byte
	data *Data
}

func NewStore(path, instance string) *Store {
	return &Store{path: path, instance: instance}
}

func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// Create writes a brand-new vault under dek and leaves the store unlocked.
func (s *Store) Create(dek []byte, d *Data) error {
	if s.Exists() {
		return errors.New("vault already exists")
	}
	d.Instance = s.instance
	d.Version = 1
	if err := s.write(dek, d); err != nil {
		return err
	}
	s.mu.Lock()
	s.dek, s.data = append([]byte(nil), dek...), d
	s.mu.Unlock()
	return nil
}

// Unlock opens the vault with dek. minVersion, if non-zero, rejects rollbacks to older vaults
// (the panel remembers the last version it saw).
func (s *Store) Unlock(dek []byte, minVersion uint64) error {
	blob, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoVault
	}
	if err != nil {
		return err
	}
	d, err := Open(dek, s.instance, blob)
	if err != nil {
		return err
	}
	if minVersion != 0 && d.Version < minVersion {
		return ErrVersion
	}
	s.mu.Lock()
	s.dek, s.data = append([]byte(nil), dek...), d
	s.mu.Unlock()
	return nil
}

// Lock wipes the key and the decrypted data from memory.
func (s *Store) Lock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.dek {
		s.dek[i] = 0
	}
	s.dek, s.data = nil, nil
}

// Key returns a copy of the data key while unlocked (used only to hand Rubi over to a verified update).
func (s *Store) Key() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil {
		return nil, ErrLocked
	}
	return append([]byte(nil), s.dek...), nil
}

func (s *Store) Unlocked() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data != nil
}

// View runs fn with read access to the decrypted data.
func (s *Store) View(fn func(*Data) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil {
		return ErrLocked
	}
	return fn(s.data)
}

// Update runs fn on a copy of the data and, if it succeeds, persists it with an incremented version.
func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return ErrLocked
	}
	raw, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	var next Data
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	next.normalize()
	if err := fn(&next); err != nil {
		return err
	}
	next.Version = s.data.Version + 1
	next.Instance = s.instance
	if err := s.write(s.dek, &next); err != nil {
		return err
	}
	s.data = &next
	return nil
}

func (s *Store) Version() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil {
		return 0, ErrLocked
	}
	return s.data.Version, nil
}

func (s *Store) write(dek []byte, d *Data) error {
	blob, err := Seal(dek, d)
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path, blob, 0o600)
}

// WriteFileAtomic writes via a temp file and rename so readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
