// Package audit is an append-only log of what Rubi did and why.
//
// Each line is JSON. While Rubi is unlocked, the detail fields (recipients, subjects, ids) are sealed
// with a key derived from the vault key, so the log reveals nothing to someone reading the disk. While
// locked, only the event name and timestamp are written; detail fields are dropped, never written plain.
// Secrets are never passed to the audit log in the first place.
package audit

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

type Fields map[string]any

type Log struct {
	path string
	mu   sync.Mutex
	key  []byte
}

func Open(path string) *Log { return &Log{path: path} }

// SetVaultKey derives the audit sealing key from the vault DEK. Pass nil on lock.
func (l *Log) SetVaultKey(dek []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.key { // the previous key, if any, doesn't linger in memory
		l.key[i] = 0
	}
	l.key = nil
	if dek == nil {
		return
	}
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, dek, nil, []byte("rubi audit v1")), k); err != nil {
		panic(err)
	}
	l.key = k
}

type line struct {
	TS     string `json:"ts"`
	Event  string `json:"event"`
	Sealed string `json:"sealed,omitempty"`
}

// Limits keep the log useful and bounded: events come partly from plugins.
const (
	maxEventName = 64
	maxFields    = 4 << 10
	maxLogSize   = 8 << 20 // then the log is moved to audit.log.1 (one old file is kept)
)

func (l *Log) Record(event string, f Fields) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(event) > maxEventName {
		event = event[:maxEventName]
	}
	if b, err := json.Marshal(f); err != nil || len(b) > maxFields {
		f = Fields{"truncated": true}
	}
	if st, err := os.Stat(l.path); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(l.path, l.path+".1")
	}
	ln := line{TS: time.Now().UTC().Format(time.RFC3339), Event: event}
	if len(f) > 0 && l.key != nil {
		if s, err := seal(l.key, ln.TS+"|"+event, f); err == nil {
			ln.Sealed = s
		}
	}
	b, err := json.Marshal(ln)
	if err != nil {
		return
	}
	fh, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.Write(append(b, '\n'))
}

func seal(key []byte, ad string, f Fields) (string, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(nonce, aead.Seal(nil, nonce, plain, []byte(ad))...)), nil
}
