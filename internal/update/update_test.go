package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		cand, cur string
		want      bool
	}{
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.2.0", false},
		{"v1.0.0", "v1.0.0-rc1", true},
		{"v1.0.0-rc2", "v1.0.0-rc1", true},
		{"v1.0.0-rc1", "v1.0.0", false},
		{"v0.2.0", "dev", false},
		{"garbage", "v0.1.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.cand, c.cur); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.cand, c.cur, got)
		}
	}
}

// fakeRelease serves a signed release whose "binary" is a shell script printing its version.
func fakeRelease(t *testing.T, version string, tamper func(name string, b []byte) []byte) (string, string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600)

	script := []byte("#!/bin/sh\necho " + version + "\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "rubi", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(script)
	_ = tw.Close()
	_ = gz.Close()
	archiveName := fmt.Sprintf("rubi-%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(buf.Bytes())
	sums := []byte(hex.EncodeToString(sum[:]) + "  " + archiveName + "\n")
	files := map[string][]byte{archiveName: buf.Bytes(), "SHA256SUMS": sums, "SHA256SUMS.sig": ed25519.Sign(priv, sums)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		b, ok := files[name]
		if !ok || !strings.Contains(r.URL.Path, "/"+version+"/") {
			http.NotFound(w, r)
			return
		}
		if tamper != nil {
			b = tamper(name, b)
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, keyFile
}

func TestDownloadVerifyInstallRollback(t *testing.T) {
	base, key := fakeRelease(t, "v0.2.0-test", nil)
	t.Setenv("RUBI_DOWNLOAD_BASE", base)
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", key)
	dir := t.TempDir()
	exe := filepath.Join(dir, "rubi")
	_ = os.WriteFile(exe, []byte("old"), 0o755)

	v, err := Download(context.Background(), "v0.1.0-test", "v0.2.0-test", dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(v, exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old" {
		t.Fatal("previous binary not kept")
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "v0.2.0-test") {
		t.Fatal("new binary not installed")
	}
	if err := Rollback(exe); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("rollback failed")
	}
}

func TestDownloadRejectsTampering(t *testing.T) {
	cases := map[string]func(string, []byte) []byte{
		"archive": func(name string, b []byte) []byte {
			if strings.HasSuffix(name, ".tar.gz") {
				return append(b, 'x')
			}
			return b
		},
		"checksums": func(name string, b []byte) []byte {
			if name == "SHA256SUMS" {
				return bytes.Replace(b, b[:4], []byte("0000"), 1)
			}
			return b
		},
	}
	for what, tamper := range cases {
		base, key := fakeRelease(t, "v0.2.0-test", tamper)
		t.Setenv("RUBI_DOWNLOAD_BASE", base)
		t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", key)
		if _, err := Download(context.Background(), "v0.1.0-test", "v0.2.0-test", t.TempDir()); err == nil {
			t.Errorf("tampered %s accepted", what)
		}
	}
}

func TestOfficialBuildsIgnoreTestOverrides(t *testing.T) {
	base, key := fakeRelease(t, "v0.2.0", nil)
	t.Setenv("RUBI_DOWNLOAD_BASE", base)
	t.Setenv("RUBI_TEST_RELEASE_KEY_FILE", key)
	t.Setenv("RUBI_UPDATE_FEED", base+"/feed.json")
	if Feed("v0.1.0") != DefaultFeed {
		t.Fatal("official build honoured a feed override")
	}
	// An official build downloads from GitHub and trusts only the embedded key, so the fake release (signed
	// with a throwaway key and served locally) must never be accepted.
	if _, err := Download(context.Background(), "v0.1.0", "v0.2.0", t.TempDir()); err == nil {
		t.Fatal("official build accepted a release signed with a test key")
	}
}
