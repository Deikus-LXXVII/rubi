// Package integrity checks that the running binary is an unmodified official release.
//
// Every release publishes SHA256SUMS (hashes of the archives and of the binaries inside them) and
// SHA256SUMS.sig, an Ed25519 signature made with the release key whose public half is embedded here and
// in install.sh. At startup Rubi downloads both for its own version, verifies the signature, and compares
// the hash of its own executable. The result is shown to the agent and in the panel.
//
// This makes casual tampering visible; it can't stop an attacker who controls the machine (see the
// security model), since such an attacker could also patch this check.
package integrity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// ReleaseKey is the public key that signs release checksums.
const ReleaseKey = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAjH/Smxzuzy59qWgOAVCSSbeNFzaZFqnFTgVPKQlgMIU=
-----END PUBLIC KEY-----`

const Repo = "Deikus-LXXVII/rubi"

// TrustedKey returns the PEM key that release checksums must be signed with. Test builds (version "dev" or
// ending in "-test") may point RUBI_TEST_RELEASE_KEY_FILE at a throwaway key so the update path can be
// exercised end to end; official releases always use the embedded ReleaseKey.
func TrustedKey(version string) []byte {
	if version == "dev" || strings.HasSuffix(version, "-test") {
		if p := os.Getenv("RUBI_TEST_RELEASE_KEY_FILE"); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				return b
			}
		}
	}
	return []byte(ReleaseKey)
}

// ReleaseBase is where release files are downloaded from (overridable only for test builds).
func ReleaseBase(version string) string {
	if version == "dev" || strings.HasSuffix(version, "-test") {
		if b := os.Getenv("RUBI_DOWNLOAD_BASE"); b != "" {
			return strings.TrimRight(b, "/")
		}
	}
	return "https://github.com/" + Repo + "/releases/download"
}

type Result struct {
	Status  string `json:"status"` // "verified" | "modified" | "unknown"
	Detail  string `json:"detail"`
	Version string `json:"version"`
}

// Check verifies the running executable against the signed checksums of version.
func Check(ctx context.Context, version string) Result {
	r := Result{Status: "unknown", Version: version}
	if version == "" || version == "dev" {
		r.Detail = "development build (not an official release)"
		return r
	}
	exe, err := os.Executable()
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	self, err := HashFile(exe)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	base := ReleaseBase(version) + "/" + version + "/"
	sums, err1 := Fetch(ctx, base+"SHA256SUMS")
	sig, err2 := Fetch(ctx, base+"SHA256SUMS.sig")
	if err := errors.Join(err1, err2); err != nil {
		r.Detail = "couldn't download release checksums: " + err.Error()
		return r
	}
	return compare(r, TrustedKey(version), sums, sig, self, "bin/rubi-"+runtime.GOOS+"-"+runtime.GOARCH)
}

// compare is the offline part of Check (tested directly).
func compare(r Result, pubPEM, sums, sig []byte, self, name string) Result {
	r.Status = "unknown"
	if err := Verify(pubPEM, sums, sig); err != nil {
		r.Status, r.Detail = "modified", "release checksums failed signature verification"
		return r
	}
	want, ok := Lookup(sums, name)
	if !ok {
		r.Detail = "no checksum for " + name + " in the release"
		return r
	}
	if want != self {
		r.Status, r.Detail = "modified", "this binary differs from the official "+r.Version+" release"
		return r
	}
	r.Status, r.Detail = "verified", "matches the signed official release"
	return r
}

// Verify checks an Ed25519 signature over msg with a PEM-encoded public key.
func Verify(pubPEM, msg, sig []byte) error {
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return errors.New("bad release key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return errors.New("release key is not Ed25519")
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("bad signature")
	}
	return nil
}

// Lookup finds the checksum for name in a SHA256SUMS file.
func Lookup(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0], true
		}
	}
	return "", false
}

// HashFile returns the hex SHA-256 of a file.
func HashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type freshKey struct{}

// WithFresh marks ctx so that FreshURL bypasses CDN caches (used right after a release announcement).
func WithFresh(ctx context.Context) context.Context { return context.WithValue(ctx, freshKey{}, true) }

// FreshURL adds a cache-busting query parameter when ctx asks for fresh data.
func FreshURL(ctx context.Context, u string) string {
	if v, _ := ctx.Value(freshKey{}).(bool); !v {
		return u
	}
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sfresh=%d", u, sep, time.Now().UnixNano())
}

// Fetch downloads a small file (up to 1 MiB).
func Fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
