package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real output of pkgutil and spctl on container-1.6.1-installer-signed.pkg.
const pkgutilOut = `Package "c.pkg":
   Status: signed by a developer certificate issued by Apple for distribution
   Notarization: trusted by the Apple notary service
   Signed with a trusted timestamp on: 2026-10-23 23:39:42 +0000
   Certificate Chain:
    1. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)
       Expires: 2030-06-04 16:44:29 +0000
       SHA256 Fingerprint:
           45 19 B4 3D 00 F5 5E C6 31 38 D4 03 DE 1F 3C 54 23 90 EA E3 D8 2E 
           59 C1 84 38 46 FB DF E1 4F 5B
       ------------------------------------------------------------------------
    2. Developer ID Certification Authority
       Expires: 2031-09-17 00:00:00 +0000
    3. Apple Root CA
       Expires: 2035-02-09 21:40:36 +0000
`

const spctlOut = `c.pkg: accepted
source=Notarized Developer ID
origin=Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)
`

func TestCheckSignature(t *testing.T) {
	if err := checkSignature(pkgutilOut, "UPBK2H6LZM"); err != nil {
		t.Fatal(err)
	}
	if err := checkAssessment(spctlOut, "UPBK2H6LZM"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ out, team string }{
		"wrong team":    {pkgutilOut, "AAAAAAAAAA"},
		"unsigned":      {"Package \"c.pkg\":\n   Status: no signature\n", "UPBK2H6LZM"},
		"not notarized": {strings.Replace(pkgutilOut, "Notarization: trusted by the Apple notary service", "Notarization: not notarized", 1), "UPBK2H6LZM"},
		"not trusted":   {strings.Replace(pkgutilOut, "issued by Apple for distribution", "but not trusted", 1), "UPBK2H6LZM"},
		"team in a later cert": {strings.Replace(pkgutilOut, "1. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)", "1. Developer ID Installer: Someone (BBBBBBBBBB)", 1) +
			"    4. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)\n", "UPBK2H6LZM"},
		"application cert": {strings.Replace(pkgutilOut, "Developer ID Installer:", "Developer ID Application:", 1), "UPBK2H6LZM"},
	} {
		if err := checkSignature(c.out, c.team); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, out := range map[string]string{
		"rejected":      "c.pkg: rejected\nsource=Unnotarized Developer ID\n",
		"not notarized": strings.Replace(spctlOut, "Notarized Developer ID", "Developer ID", 1),
		"other origin":  strings.Replace(spctlOut, "UPBK2H6LZM", "BBBBBBBBBB", 1),
	} {
		if err := checkAssessment(out, "UPBK2H6LZM"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- download ----

type dl struct {
	t       *testing.T
	body    []byte
	rel     Release
	srv     *httptest.Server
	cdn     *httptest.Server
	handler http.HandlerFunc
}

// newDownload serves body through a redirect: srv (github.com) -> cdn (the asset host).
func newDownload(t *testing.T, body []byte, cdn http.HandlerFunc) *dl {
	t.Helper()
	d := &dl{t: t, body: body}
	if cdn == nil {
		cdn = func(w http.ResponseWriter, r *http.Request) { w.Write(body) }
	}
	d.cdn = httptest.NewTLSServer(cdn)
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, d.cdn.URL+"/asset", http.StatusFound)
	}))
	t.Cleanup(d.srv.Close)
	t.Cleanup(d.cdn.Close)
	sum := sha256.Sum256(body)
	d.rel = Release{Version: "1.6.1", URL: d.srv.URL + "/pkg", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Team: "UPBK2H6LZM"}
	return d
}

// opts points a Setup at the test servers (both use a self-signed certificate; the client trusts it).
func (d *dl) opts(o *Options) {
	d.cdn.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(d.srv.Certificate())
	o.HTTP = d.cdn.Client()
	o.Release = d.rel
	o.DownloadHosts = []string{strings.TrimPrefix(d.srv.URL, "https://")}
	o.RedirectHosts = []string{strings.TrimPrefix(d.cdn.URL, "https://")}
}

func TestDownload(t *testing.T) {
	body := []byte(strings.Repeat("pkg-bytes-", 1000))
	t.Run("ok through the redirect", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts)
		dir := filepath.Join(t.TempDir(), "setup")
		p, err := e.s.download(context.Background(), dir, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(body) {
			t.Error("content differs")
		}
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", st.Mode())
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v", st.Mode())
		}
		// a second call keeps the matching file and downloads nothing
		d.cdn.Close()
		if p2, err := e.s.download(context.Background(), dir, func(string) {}); err != nil || p2 != p {
			t.Errorf("second call: %v %v", p2, err)
		}
	})
	t.Run("wrong hash deletes the file", func(t *testing.T) {
		d := newDownload(t, body, nil)
		d.rel.SHA256 = strings.Repeat("0", 64)
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		_, err := e.s.download(context.Background(), dir, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("err %v", err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("left behind: %v", ents)
		}
	})
	t.Run("too large", func(t *testing.T) {
		for name, h := range map[string]http.HandlerFunc{
			"declared": func(w http.ResponseWriter, r *http.Request) { w.Write(append(body, body...)) },
			"streamed": func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < 3; i++ {
					w.Write(body)
					w.(http.Flusher).Flush()
				}
			},
		} {
			d := newDownload(t, body, h)
			e := newTestEnv(t, d.opts)
			dir := t.TempDir()
			if _, err := e.s.download(context.Background(), dir, func(string) {}); err == nil || !strings.Contains(err.Error(), "larger") {
				t.Errorf("%s: err %v", name, err)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("%s: left behind: %v", name, ents)
			}
		}
	})
	t.Run("too small", func(t *testing.T) {
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) { w.Write(body[:100]) })
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		if _, err := e.s.download(context.Background(), dir, func(string) {}); err == nil || !strings.Contains(err.Error(), "size") {
			t.Errorf("err %v", err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("left behind: %v", ents)
		}
	})
	t.Run("redirect to a host that isn't allowed", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts, func(o *Options) { o.RedirectHosts = []string{"release-assets.githubusercontent.com"} })
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't allowed") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("redirect to http", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
		defer plain.Close()
		d := newDownload(t, body, nil)
		d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) })
		e := newTestEnv(t, d.opts, func(o *Options) { o.RedirectHosts = append(o.RedirectHosts, strings.TrimPrefix(plain.URL, "http://")) })
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil {
			t.Error("followed a redirect to plain http")
		}
	})
	t.Run("the start must be an allowed https host", func(t *testing.T) {
		d := newDownload(t, body, nil)
		for _, u := range []string{"http://github.com/x.pkg", "https://example.com/x.pkg", "https://user@github.com/x.pkg", "ftp://github.com/x"} {
			e := newTestEnv(t, d.opts, func(o *Options) { o.Release.URL = u; o.DownloadHosts = []string{"github.com"} })
			if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil {
				t.Errorf("%s accepted", u)
			}
		}
	})
	t.Run("stalls", func(t *testing.T) {
		old := stallLimit
		stallLimit = 300 * time.Millisecond
		defer func() { stallLimit = old }()
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) {
			w.Write(body[:100])
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		start := time.Now()
		if _, err := e.s.download(context.Background(), dir, func(string) {}); err == nil || !strings.Contains(err.Error(), "stalled") {
			t.Errorf("err %v", err)
		}
		if time.Since(start) > 3*time.Second {
			t.Error("the stall wasn't noticed in time")
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("left behind: %v", ents)
		}
	})
	t.Run("overall limit", func(t *testing.T) {
		old := downloadLimit
		downloadLimit = 300 * time.Millisecond
		defer func() { downloadLimit = old }()
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < 100 && r.Context().Err() == nil; i++ { // a trickle, so it never stalls
				w.Write(body[:1])
				w.(http.Flusher).Flush()
				time.Sleep(50 * time.Millisecond)
			}
		})
		e := newTestEnv(t, d.opts)
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil || !strings.Contains(err.Error(), "too long") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("server error", func(t *testing.T) {
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
		e := newTestEnv(t, d.opts)
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a stale wrong file is replaced", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, d.rel.fileName()), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.download(context.Background(), dir, func(string) {}); err != nil {
			t.Fatal(err)
		}
	})
}

// ---- admin install ----

// ---- the container job ----

func TestInstallFailureShowsOnlyFixedLines(t *testing.T) {
	if got := installFailure("0:1: execution error: the package changed after it was checked (11)"); !strings.Contains(got, "the package changed after it was checked") {
		t.Errorf("known reason lost: %q", got)
	}
	got := installFailure("installer: token=sk-abcdef0123456789abcdef0123456789 leaked")
	if strings.Contains(got, "sk-") || strings.Contains(got, "leaked") {
		t.Errorf("unknown output shown: %q", got)
	}
}
