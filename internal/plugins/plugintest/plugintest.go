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
	"sort"
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
	slow     time.Duration     // delay before serving a plugin archive (SlowDownloads)
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
func (r *Registry) Source() string { return r.SourceOf("demo") }

// SourceOf is the source URL of the plugin with this id (see PublishAs) on this registry.
func (r *Registry) SourceOf(id string) string { return r.Server.URL + "/" + id }

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	p := req.URL.Path
	if id, rest, ok := strings.Cut(strings.TrimPrefix(p, "/"), "/latest/"); ok && r.latest[id] != "" {
		p = "/" + id + "/" + r.latest[id] + "/" + rest
	}
	b, ok := r.files[p]
	slow := r.slow
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}
	if slow > 0 && strings.HasSuffix(p, ".tar.gz") && len(b) > 1 {
		// Half now, half later: the file is being written for a while, as with a slow connection.
		_, _ = w.Write(b[:len(b)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(slow)
		b = b[len(b)/2:]
	}
	_, _ = w.Write(b)
}

// Publish builds the demo plugin at version, signs the release with key (nil: the registry's publisher)
// and makes it the latest. mutate may change the manifest before it is published.
func (r *Registry) Publish(version string, key ed25519.PrivateKey, mutate func(m map[string]any)) (sumsSHA string) {
	return r.PublishAs("demo", version, key, mutate)
}

// PublishAs is Publish for a plugin with another id: the same demo plugin under that id and the name
// "Demo <id>", for tests that need several plugins. Source URLs are SourceOf(id).
func (r *Registry) PublishAs(id, version string, key ed25519.PrivateKey, mutate func(m map[string]any)) (sumsSHA string) {
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
	m["id"], m["name"] = id, pluginName(id)
	for list, field := range map[string]string{"actions": "kind", "tools": "name"} { // they start with the plugin's id
		items, _ := m[list].([]any)
		for _, it := range items {
			if im, ok := it.(map[string]any); ok {
				if v, _ := im[field].(string); strings.HasPrefix(v, "demo.") || strings.HasPrefix(v, "demo_") {
					im[field] = id + v[len("demo"):]
				}
			}
		}
	}
	if mutate != nil {
		mutate(m)
	}
	manifest, _ := json.MarshalIndent(m, "", "  ")
	exe, _ := os.ReadFile(bin)
	archive := tarGz(r.t, map[string][]byte{"demo": exe})
	name := fmt.Sprintf("%s-%s-%s.tar.gz", id, runtime.GOOS, runtime.GOARCH)
	sums := fmt.Sprintf("%s  rubi-plugin.json\n%s  %s\n", hexSum(manifest), hexSum(archive), name)
	sig := ed25519.Sign(key, []byte(sums))

	r.mu.Lock()
	defer r.mu.Unlock()
	base := "/" + id + "/" + version + "/"
	r.files[base+"rubi-plugin.json"] = manifest
	r.files[base+name] = archive
	r.files[base+"SHA256SUMS"] = []byte(sums)
	r.files[base+"SHA256SUMS.sig"] = sig
	r.latest[id] = version
	return hexSum([]byte(sums))
}

// PublishRubi makes version the latest Rubi release on this registry: the release feed and the signed checksums
// (the release archive itself is not served: tests replace the installing step). It points this process at them,
// which only a build whose version is "dev" or ends in "-test" honors.
func (r *Registry) PublishRubi(version string) {
	r.t.Helper()
	sums := []byte(fmt.Sprintf("%s  rubi-%s-%s-%s.tar.gz\n", hexSum([]byte(version)), version, runtime.GOOS, runtime.GOARCH))
	feed, _ := json.Marshal(map[string]string{"version": version, "notes_url": r.Server.URL + "/rubi/notes/" + version})
	r.mu.Lock()
	r.files["/rubi/latest.json"] = feed
	r.files["/rubi/"+version+"/SHA256SUMS"] = sums
	r.files["/rubi/"+version+"/SHA256SUMS.sig"] = ed25519.Sign(r.releaseKey, sums)
	r.mu.Unlock()
	r.t.Setenv("RUBI_UPDATE_FEED", r.Server.URL+"/rubi/latest.json")
	r.t.Setenv("RUBI_DOWNLOAD_BASE", r.Server.URL+"/rubi")
}

// SlowDownloads makes every plugin archive take d to arrive, so that downloads of two requests overlap.
func (r *Registry) SlowDownloads(d time.Duration) {
	r.mu.Lock()
	r.slow = d
	r.mu.Unlock()
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
	r.ReviewAs("demo", version, sumsSHA, minRubi)
}

// ReviewAs lists a version published with PublishAs in the catalog.
func (r *Registry) ReviewAs(id, version, sumsSHA, minRubi string) {
	r.mu.Lock()
	r.reviewed[id] = append(r.reviewed[id], map[string]string{"version": version, "sums_sha256": sumsSHA, "min_rubi": minRubi})
	r.mu.Unlock()
	r.writeCatalog()
}

func (r *Registry) writeCatalog() {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []any
	var ids []string
	for id := range r.reviewed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if v := r.reviewed[id]; len(v) > 0 {
			list = append(list, map[string]any{"id": id, "name": pluginName(id), "summary": "Test plugin.",
				"publisher": map[string]string{"name": "Test Publisher", "key": r.PubKey},
				"source":    r.Server.URL + "/" + id, "versions": v})
		}
	}
	cat, _ := json.Marshal(map[string]any{"schema": 1, "updated_at": time.Now().UTC(), "plugins": list})
	r.files["/marketplace/catalog.json"] = cat
	r.files["/marketplace/catalog.json.sig"] = ed25519.Sign(r.releaseKey, cat)
}

// pluginName is the display name of a test plugin.
func pluginName(id string) string {
	if id == "demo" {
		return "Demo"
	}
	return "Demo " + id
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
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed) // the archive holds a whole executable: speed over size
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
