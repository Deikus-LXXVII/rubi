// Package libsig is the signature on an admitted library skill (docs/design/agent-roles.md section 8). Rubi's
// core signs when the curator admits a version; Rubi Home verifies with the library public key it pinned.
//
// The signed message is Prefix followed by this JSON, written with no spaces and in this field order:
//
//	{"id","version","sha256","admitted_by","admitted_at","scan_verdict","scanner_version","self_admitted","acked"}
//
// The signature travels as a Token: URL-safe base64 (no padding) of a JSON object that carries the admission
// facts and the Ed25519 signature, so a skill in a role needs only its id, version, hash and this one string.
package libsig

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
)

// Prefix separates this signature from any other use of the key.
const Prefix = "rubi-skill-library-v2\n"

// MaxToken is the longest Token Rubi issues.
const MaxToken = 2000

// Claims are the facts a signature covers.
type Claims struct {
	ID             string
	Version        int
	SHA256         string
	AdmittedBy     string
	AdmittedAt     string // RFC 3339, UTC, seconds
	ScanVerdict    string // "pass" or "flag"
	ScannerVersion string
	SelfAdmitted   bool
	// Acked are the scan rule ids the curator acknowledged at admission (sorted). Rubi Home holds a library skill
	// that trips a flag not among them.
	Acked []string
}

type message struct {
	ID             string   `json:"id"`
	Version        int      `json:"version"`
	SHA256         string   `json:"sha256"`
	AdmittedBy     string   `json:"admitted_by"`
	AdmittedAt     string   `json:"admitted_at"`
	ScanVerdict    string   `json:"scan_verdict"`
	ScannerVersion string   `json:"scanner_version"`
	SelfAdmitted   bool     `json:"self_admitted"`
	Acked          []string `json:"acked"`
}

// Message is the exact byte string that is signed.
func Message(c Claims) []byte {
	var b bytes.Buffer
	b.WriteString(Prefix)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	m := message(c)
	m.Acked = append([]string{}, c.Acked...)
	sort.Strings(m.Acked)
	_ = enc.Encode(m)
	return bytes.TrimRight(b.Bytes(), "\n")
}

type token struct {
	AdmittedBy     string   `json:"admitted_by"`
	AdmittedAt     string   `json:"admitted_at"`
	ScanVerdict    string   `json:"scan_verdict"`
	ScannerVersion string   `json:"scanner_version"`
	SelfAdmitted   bool     `json:"self_admitted"`
	Acked          []string `json:"acked,omitempty"`
	Sig            string   `json:"sig"`
}

// Sign returns the Token for c.
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("bad library key")
	}
	t := token{c.AdmittedBy, c.AdmittedAt, c.ScanVerdict, c.ScannerVersion, c.SelfAdmitted, c.Acked,
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, Message(c)))}
	raw, _ := json.Marshal(t)
	out := base64.RawURLEncoding.EncodeToString(raw)
	if len(out) > MaxToken {
		return "", errors.New("signature token too long")
	}
	return out, nil
}

// Verify checks tok for the skill (id, version, sha256) against pub and returns what it attests.
func Verify(pub ed25519.PublicKey, id, version, sha256, tok string) (Claims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Claims{}, errors.New("no library key is pinned on this computer")
	}
	n, err := strconv.Atoi(version)
	if err != nil || n < 1 {
		return Claims{}, errors.New("bad version")
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(tok) > MaxToken {
		return Claims{}, errors.New("malformed signature")
	}
	var t token
	if err := json.Unmarshal(raw, &t); err != nil {
		return Claims{}, errors.New("malformed signature")
	}
	sig, err := base64.RawURLEncoding.DecodeString(t.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, errors.New("malformed signature")
	}
	c := Claims{ID: id, Version: n, SHA256: sha256, AdmittedBy: t.AdmittedBy, AdmittedAt: t.AdmittedAt,
		ScanVerdict: t.ScanVerdict, ScannerVersion: t.ScannerVersion, SelfAdmitted: t.SelfAdmitted, Acked: t.Acked}
	if !ed25519.Verify(pub, Message(c), sig) {
		return Claims{}, fmt.Errorf("the signature doesn't match skill %s version %d", id, n)
	}
	if c.ScanVerdict != "pass" && c.ScanVerdict != "flag" {
		return Claims{}, errors.New("the signature attests no passing scan")
	}
	return c, nil
}

// RevocationPrefix separates a revocation list's signature from a skill's.
const RevocationPrefix = "rubi-skill-revocations-v1\n"

// RevocationMessage is the byte string that is signed for a list: the time it was issued (Unix milliseconds; a
// computer keeps only the newest list it has seen) and the withdrawn versions as "id@version", sorted.
func RevocationMessage(issuedAt int64, revoked []string) []byte {
	list := append([]string{}, revoked...)
	sort.Strings(list)
	var b bytes.Buffer
	b.WriteString(RevocationPrefix)
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(struct {
		IssuedAt int64    `json:"issued_at"`
		Revoked  []string `json:"revoked"`
	}{issuedAt, list})
	return bytes.TrimRight(b.Bytes(), "\n")
}

// SignRevocations signs a revocation list; the result is URL-safe base64 without padding.
func SignRevocations(priv ed25519.PrivateKey, issuedAt int64, revoked []string) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("bad library key")
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, RevocationMessage(issuedAt, revoked))), nil
}

// VerifyRevocations checks a signed list against pub.
func VerifyRevocations(pub ed25519.PublicKey, issuedAt int64, revoked []string, sig string) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("no library key is pinned on this computer")
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(raw) != ed25519.SignatureSize || !ed25519.Verify(pub, RevocationMessage(issuedAt, revoked), raw) {
		return errors.New("the revocation list's signature doesn't match")
	}
	return nil
}
