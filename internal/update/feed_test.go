package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// feedRig serves files by path and trusts the throwaway key that signs them.
type feedRig struct {
	priv  ed25519.PrivateKey
	files map[string][]byte
	url   string
}

func newFeedRig(t *testing.T) *feedRig {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600)
	r := &feedRig{priv: priv, files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, ok := r.files[req.URL.Path]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", keyFile)
	t.Setenv("RUBI_UPDATE_FEED", srv.URL+"/latest.json")
	t.Setenv("RUBI_DOWNLOAD_BASE", srv.URL+"/dl")
	return r
}

func (r *feedRig) feed(version string, sign bool) {
	b := []byte(`{"version":"` + version + `"}`)
	r.files["/latest.json"] = b
	delete(r.files, "/latest.json.sig")
	if sign {
		r.files["/latest.json.sig"] = ed25519.Sign(r.priv, b)
	}
}

func (r *feedRig) sums(version, body string) {
	b := []byte(body)
	r.files["/dl/"+version+"/SHA256SUMS"] = b
	r.files["/dl/"+version+"/SHA256SUMS.sig"] = ed25519.Sign(r.priv, b)
}

func TestLatestTakesTheVersionOnlyFromTheSignedFeed(t *testing.T) {
	r := newFeedRig(t)
	r.feed("v0.2.0", true)
	rel, err := Latest(context.Background(), "v0.1.0-test")
	if err != nil || rel.Version != "v0.2.0" {
		t.Fatalf("signed feed: %v %v", rel, err)
	}

	r.feed("v0.2.0", false) // no .sig at all
	if _, err := Latest(context.Background(), "v0.1.0-test"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("feed without a signature accepted: %v", err)
	}

	r.feed("v0.2.0", true)
	r.files["/latest.json"] = []byte(`{"version":"v0.1.0"}`) // the version was changed, the old signature stays
	if _, err := Latest(context.Background(), "v0.1.0-test"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("tampered feed accepted: %v", err)
	}

	r.feed("v0.2.0", true)
	_, other, _ := ed25519.GenerateKey(rand.Reader) // a signature by another key
	r.files["/latest.json.sig"] = ed25519.Sign(other, r.files["/latest.json"])
	if _, err := Latest(context.Background(), "v0.1.0-test"); err == nil {
		t.Fatal("feed signed by another key accepted")
	}
}

func TestChecksumsMustNameTheRequestedVersion(t *testing.T) {
	r := newFeedRig(t)
	name := func(v string) string { return "rubi-" + v + "-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz" }
	h := strings.Repeat("a", 64)

	// A genuine, signed list of another release served for v0.14.0.
	r.sums("v0.14.0", h+"  "+name("v0.13.0")+"\n")
	if _, err := Checksums(context.Background(), "v0.1.0-test", "v0.14.0"); err == nil {
		t.Fatal("a signed list of v0.13.0 accepted for v0.14.0")
	}
	// The right archive, but another release's asset beside it.
	r.sums("v0.14.0", h+"  "+name("v0.14.0")+"\n"+h+"  rubi-home-v0.13.0-macos-universal.zip\n")
	if _, err := Checksums(context.Background(), "v0.1.0-test", "v0.14.0"); err == nil {
		t.Fatal("a list with an asset of v0.13.0 accepted for v0.14.0")
	}
	// A pre-release's archive is not the release's.
	r.sums("v0.14.0", h+"  "+name("v0.14.0-rc1")+"\n")
	if _, err := Checksums(context.Background(), "v0.1.0-test", "v0.14.0"); err == nil {
		t.Fatal("a list for v0.14.0-rc1 accepted for v0.14.0")
	}
	// Nothing versioned at all: the signed list does not bind the version.
	r.sums("v0.14.0", h+"  bin/rubi-linux-amd64\n")
	if _, err := Checksums(context.Background(), "v0.1.0-test", "v0.14.0"); err == nil {
		t.Fatal("a list without the archive accepted")
	}
	// The real shape: versioned names for this version, unversioned binaries ignored.
	r.sums("v0.14.0", h+"  "+name("v0.14.0")+"\n"+h+"  rubi-home-v0.14.0-windows-amd64.zip\n"+h+"  bin/rubi-linux-amd64\n")
	if _, err := Checksums(context.Background(), "v0.1.0-test", "v0.14.0"); err != nil {
		t.Fatalf("a good list refused: %v", err)
	}
}

func TestDownloadRefusesAnOlderOrSameGenuineRelease(t *testing.T) {
	// An official build never reaches the network for this: the guard comes first.
	for _, v := range []string{"v0.1.0", "v0.2.0", "v0.2.0-rc1"} {
		_, err := Download(context.Background(), "v0.2.0", v, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "not newer") {
			t.Errorf("Download(v0.2.0 -> %s) = %v", v, err)
		}
	}
	// A test build may go anywhere (the rig above relies on it).
	r := newFeedRig(t)
	r.sums("v0.1.0", "")
	if _, err := Download(context.Background(), "v0.2.0-test", "v0.1.0", t.TempDir()); err != nil && strings.Contains(err.Error(), "not newer") {
		t.Fatalf("a test build was held to the downgrade guard: %v", err)
	}
}
