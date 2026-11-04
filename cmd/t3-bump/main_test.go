package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/t3bump"
)

type noRunner struct{ calls int }

func (n *noRunner) Run(context.Context, string, []string, string, ...string) ([]byte, error) {
	n.calls++
	return nil, nil
}

// serve is GitHub and npm for one nightly.
func serve(t *testing.T, tag string) (*httptest.Server, *[]string) {
	var hits []string
	ver := strings.TrimPrefix(tag, "v")
	tarball := []byte("tarball")
	h := sha512.Sum512(tarball)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch {
		case r.URL.Path == "/repos/pingdotgg/t3code/releases":
			json.NewEncoder(w).Encode([]any{map[string]any{"tag_name": tag, "assets": []any{map[string]any{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/sums"}}}})
		case r.URL.Path == "/sums":
			for i, p := range t3bump.Platforms {
				fmt.Fprintf(w, "%064x  %s\n", i+1, t3bump.ArchiveName(ver, p))
			}
		case strings.HasPrefix(r.URL.Path, "/repos/pingdotgg/t3code/commits/"):
			json.NewEncoder(w).Encode(map[string]string{"sha": strings.Repeat("a", 40)})
		case r.URL.Path == "/t3/"+ver:
			json.NewEncoder(w).Encode(map[string]any{"name": "t3", "version": ver, "dist": map[string]string{"tarball": srv.URL + "/tgz", "integrity": "sha512-" + base64.StdEncoding.EncodeToString(h[:])}})
		case r.URL.Path == "/tgz":
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func root(t *testing.T) string {
	d := t.TempDir()
	os.MkdirAll(filepath.Join(d, "t3", "contract"), 0o755)
	b, err := os.ReadFile(filepath.Join("..", "..", "t3", "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "t3", "upstream.json"), b, 0o644)
	os.WriteFile(filepath.Join(d, "t3", "contract", "tools.json"), []byte(`{}`), 0o644)
	return d
}

func TestRunWritesThePinAndTheReport(t *testing.T) {
	srv, hits := serve(t, "v0.3.0-nightly.20270301.1")
	d := root(t)
	report := filepath.Join(t.TempDir(), "report.md")
	var out, errb bytes.Buffer
	nr := &noRunner{}
	code := run(context.Background(), []string{"-root", d, "-report", report, "-github-api", srv.URL, "-npm-registry", srv.URL, "-skip-contract", "-date", "2027-03-04"},
		func(k string) string {
			if k == "GITHUB_TOKEN" {
				return "tok"
			}
			return ""
		}, &out, &errb, nr)
	if code != 0 || nr.calls != 0 {
		t.Fatalf("exit %d, %d commands: %s %s", code, nr.calls, out.String(), errb.String())
	}
	up, _ := os.ReadFile(filepath.Join(d, "t3", "upstream.json"))
	if !strings.Contains(string(up), `"tag": "v0.3.0-nightly.20270301.1"`) || !strings.Contains(string(up), `"checked": "2027-03-04"`) {
		t.Errorf("upstream.json:\n%s", up)
	}
	rep, _ := os.ReadFile(report)
	if !strings.Contains(string(rep), "Move the T3 pin to `v0.3.0-nightly.20270301.1`") || !strings.Contains(out.String(), "report in") {
		t.Errorf("report %s out %s", rep, out.String())
	}
	if len(*hits) == 0 {
		t.Error("the stand-in was never asked")
	}
	// a second run finds nothing new
	out.Reset()
	if code := run(context.Background(), []string{"-root", d, "-report", report, "-github-api", srv.URL, "-npm-registry", srv.URL, "-skip-contract"},
		func(string) string { return "" }, &out, &errb, nr); code != 0 || !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("second run: %d %s", code, out.String())
	}
	if rep, _ := os.ReadFile(report); !strings.Contains(string(rep), "up to date") {
		t.Errorf("report: %s", rep)
	}
}

func TestRunErrorsAreExitCodeOne(t *testing.T) {
	d := root(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer dead.Close()
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"-root", d, "-report", filepath.Join(t.TempDir(), "r.md"), "-github-api", dead.URL, "-npm-registry", dead.URL},
		func(string) string { return "" }, &out, &errb, &noRunner{}); code != 1 || !strings.Contains(errb.String(), "HTTP 500") {
		t.Errorf("exit %d: %s", code, errb.String())
	}
	if code := run(context.Background(), []string{"-nope"}, func(string) string { return "" }, &out, &errb, &noRunner{}); code != 2 {
		t.Errorf("bad flag: %d", code)
	}
	if code := run(context.Background(), []string{"-root", t.TempDir(), "-github-api", dead.URL}, func(string) string { return "" }, &out, &errb, &noRunner{}); code != 1 {
		t.Errorf("no upstream.json: %d", code)
	}
	// an address that is not https is refused before anything is fetched
	if code := run(context.Background(), []string{"-root", d, "-github-api", "http://example.com", "-report", filepath.Join(t.TempDir(), "r.md")},
		func(string) string { return "" }, &out, &errb, &noRunner{}); code != 1 || !strings.Contains(errb.String(), "refusing") {
		t.Errorf("plain http: %d %s", code, errb.String())
	}
}
