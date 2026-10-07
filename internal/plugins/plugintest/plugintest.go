// Package plugintest builds signed plugin releases and a signed catalog for tests.
package plugintest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Registry is a fake release host plus the Rubi catalog, signed with throwaway keys.
type Registry struct {
	t          *testing.T
	Server     *httptest.Server
	Publisher  ed25519.PrivateKey
	PubKey     string // base64 SPKI DER
	releaseKey ed25519.PrivateKey

	mu       sync.Mutex
	files    map[string][]byte // path -> content
	latest   map[string]string // plugin -> version
	reviewed map[string][]map[string]string
}

// New starts a registry and points this process's Rubi (a dev build) at its catalog and release key.
func New(t *testing.T) *Registry {
	t.Helper()
	_, pubPriv, _ := ed25519.GenerateKey(rand.Reader)
	relPub, relPriv, _ := ed25519.GenerateKey(rand.Reader)
	r := &Registry{t: t, Publisher: pubPriv, releaseKey: relPriv, files: map[string][]byte{},
		latest: map[string]string{}, reviewed: map[string][]map[string]string{}}
	r.PubKey = KeyB64(pubPriv.Public().(ed25519.PublicKey))
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)

	der, _ := x509.MarshalPKIXPublicKey(relPub)
	keyFile := filepath.Join(t.TempDir(), "release.pem")
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600)
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", keyFile)
	t.Setenv("RUBI_MARKETPLACE_URL", r.Server.URL+"/marketplace/catalog.json")
	r.writeCatalog()
	return r
}

// KeyB64 encodes a public key the way manifests carry it.
func KeyB64(pub ed25519.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return base64.StdEncoding.EncodeToString(der)
}

// Source is the demo plugin's source URL on this registry.
func (r *Registry) Source() string { return r.Server.URL + "/demo" }

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	p := req.URL.Path
	if rest, ok := strings.CutPrefix(p, "/demo/latest/"); ok {
		p = "/demo/" + r.latest["demo"] + "/" + rest
	}
	b, ok := r.files[p]
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, _ = w.Write(b)
}

// Publish builds the demo plugin at version, signs the release with key (nil: the registry's publisher)
// and makes it the latest. mutate may change the manifest before it is published.
func (r *Registry) Publish(version string, key ed25519.PrivateKey, mutate func(m map[string]any)) (sumsSHA string) {
	r.t.Helper()
	if key == nil {
		key = r.Publisher
	}
	bin := r.build(KeyB64(key.Public().(ed25519.PublicKey)))
	out, err := exec.Command(bin, "--manifest").Output()
	if err != nil {
		r.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	m["version"] = version
	if mutate != nil {
		mutate(m)
	}
	manifest, _ := json.MarshalIndent(m, "", "  ")
	exe, _ := os.ReadFile(bin)
	archive := tarGz(r.t, map[string][]byte{"demo": exe})
	name := fmt.Sprintf("demo-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%s  rubi-plugin.json\n%s  %s\n", hexSum(manifest), hexSum(archive), name)
	sig := ed25519.Sign(key, []byte(sums))

	r.mu.Lock()
	defer r.mu.Unlock()
	base := "/demo/" + version + "/"
	r.files[base+"rubi-plugin.json"] = manifest
	r.files[base+name] = archive
	r.files[base+"SHA256SUMS"] = []byte(sums)
	r.files[base+"SHA256SUMS.sig"] = sig
	r.latest["demo"] = version
	return hexSum([]byte(sums))
}

// Tamper replaces the archive of a published version (its checksum no longer matches).
func (r *Registry) Tamper(version string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := fmt.Sprintf("/demo/%s/demo-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	r.files[name] = append([]byte(nil), append(r.files[name], 0)...)
}

// Review lists a published version in the catalog.
func (r *Registry) Review(version, sumsSHA, minRubi string) {
	r.mu.Lock()
	r.reviewed["demo"] = append(r.reviewed["demo"], map[string]string{"version": version, "sums_sha256": sumsSHA, "min_rubi": minRubi})
	r.mu.Unlock()
	r.writeCatalog()
}

func (r *Registry) writeCatalog() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []any
	if v := r.reviewed["demo"]; len(v) > 0 {
		list = append(list, map[string]any{"id": "demo", "name": "Demo", "summary": "Test plugin.",
			"publisher": map[string]string{"name": "Test Publisher", "key": r.PubKey},
			"source":    r.Server.URL + "/demo", "versions": v})
	}
	cat, _ := json.Marshal(map[string]any{"schema": 1, "updated_at": time.Now().UTC(), "plugins": list})
	r.files["/marketplace/catalog.json"] = cat
	r.files["/marketplace/catalog.json.sig"] = ed25519.Sign(r.releaseKey, cat)
}

var (
	buildMu    sync.Mutex
	buildCache = map[string]string{}
)

func (r *Registry) build(pubKey string) string {
	buildMu.Lock()
	defer buildMu.Unlock()
	if b, ok := buildCache[pubKey]; ok {
		return b
	}
	dir, err := os.MkdirTemp("", "rubi-demo-plugin-")
	if err != nil {
		r.t.Fatal(err)
	}
	bin := filepath.Join(dir, "demo")
	cmd := exec.Command("go", "build", "-ldflags", "-X main.PublisherKey="+pubKey, "-o", bin,
		"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest/demo")
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("build demo plugin: %v\n%s", err, out)
	}
	buildCache[pubKey] = bin
	return bin
}

func tarGz(t *testing.T, files map[string][]byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, b := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
