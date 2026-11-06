package t3bump

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeWorld is GitHub's API and the npm registry, in one httptest server. Nothing here reaches the real ones.
type fakeWorld struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	releases []map[string]any
	sums     map[string]string // tag -> SHA256SUMS text
	tarballs map[string][]byte // version -> npm tarball
	integr   map[string]string // version -> integrity the registry claims (default: the tarball's own)
	commits  map[string]string // tag -> commit
	status   map[string]int    // path prefix -> forced status
	auth     map[string]string // path -> Authorization header seen
	hits     []string
}

func newWorld(t *testing.T) *fakeWorld {
	w := &fakeWorld{t: t, sums: map[string]string{}, tarballs: map[string][]byte{}, integr: map[string]string{}, commits: map[string]string{},
		status: map[string]int{}, auth: map[string]string{}}
	w.srv = httptest.NewServer(http.HandlerFunc(w.serve))
	t.Cleanup(w.srv.Close)
	return w
}

func sri(b []byte) string {
	h := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(h[:])
}

// add publishes a nightly with sums for all platforms, a tarball and a commit.
func (w *fakeWorld) add(tag string, opts ...func(*release)) {
	r := &release{tag: tag, assets: true}
	for _, o := range opts {
		o(r)
	}
	ver := strings.TrimPrefix(tag, "v")
	w.mu.Lock()
	defer w.mu.Unlock()
	rel := map[string]any{"tag_name": tag, "draft": r.draft, "prerelease": true}
	var assets []map[string]any
	if r.assets {
		assets = append(assets, map[string]any{"name": "SHA256SUMS", "browser_download_url": w.srv.URL + "/dl/" + tag + "/SHA256SUMS"})
	}
	rel["assets"] = assets
	w.releases = append(w.releases, rel)
	var sb strings.Builder
	for i, p := range Platforms {
		if r.dropPlatform == p {
			continue
		}
		fmt.Fprintf(&sb, "%064x  %s\n", i+1+len(tag), ArchiveName(ver, p))
	}
	fmt.Fprintf(&sb, "%064x  t3-%s-win32-arm64.zip\n", 99, ver) // a platform Rubi Home has no pin for
	w.sums[tag] = sb.String() + r.extraSums
	w.tarballs[ver] = []byte("tarball of " + ver)
	w.commits[tag] = fmt.Sprintf("%040x", len(tag)*7919)
}

type release struct {
	tag          string
	draft        bool
	assets       bool
	dropPlatform string
	extraSums    string
}

func (w *fakeWorld) serve(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	w.hits = append(w.hits, r.URL.Path)
	w.auth[r.URL.Path] = r.Header.Get("Authorization")
	for prefix, code := range w.status {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.mu.Unlock()
			http.Error(rw, "forced", code)
			return
		}
	}
	defer w.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/pingdotgg/t3code/releases":
		_ = json.NewEncoder(rw).Encode(w.releases)
	case strings.HasPrefix(r.URL.Path, "/repos/pingdotgg/t3code/commits/"):
		tag := strings.TrimPrefix(r.URL.Path, "/repos/pingdotgg/t3code/commits/")
		c, ok := w.commits[tag]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]string{"sha": c})
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/dl/"), "/")
		s, ok := w.sums[parts[0]]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write([]byte(s))
	case strings.HasPrefix(r.URL.Path, "/t3/"):
		ver := strings.TrimPrefix(r.URL.Path, "/t3/")
		tb, ok := w.tarballs[ver]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		in := w.integr[ver]
		if in == "" {
			in = sri(tb)
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"name": "t3", "version": ver, "dist": map[string]string{"tarball": w.srv.URL + "/tarballs/t3-" + ver + ".tgz", "integrity": in}})
	case strings.HasPrefix(r.URL.Path, "/tarballs/"):
		ver := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/tarballs/t3-"), ".tgz")
		tb, ok := w.tarballs[ver]
		if !ok {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(tb)
	default:
		http.NotFound(rw, r)
	}
}

func (w *fakeWorld) client() *Client {
	return &Client{API: w.srv.URL, NPM: w.srv.URL, Token: "ghp_test_token_not_real"}
}

func TestParseTag(t *testing.T) {
	for tag, ok := range map[string]bool{
		"v0.1.7-nightly.20261102.4093": true, "v1.20.3-nightly.20271231.1": true,
		"v0.1.7": false, "0.1.7-nightly.20261102.4093": false, "v0.1.7-nightly.20261340.1": false, "v0.1.7-nightly.20261102": false,
		"v0.1.7-nightly.20261102.4093-rc": false, "v0.1-nightly.20261102.1": false, "": false, "vX.1.7-nightly.20261102.1": false,
	} {
		if _, got := ParseTag(tag); got != ok {
			t.Errorf("ParseTag(%q) = %v, want %v", tag, got, ok)
		}
	}
	a, _ := ParseTag("v0.1.7-nightly.20261102.9")
	b, _ := ParseTag("v0.1.7-nightly.20261102.10")
	c, _ := ParseTag("v0.1.7-nightly.20261103.1")
	d, _ := ParseTag("v0.1.8-nightly.20261102.10")
	if !a.Less(b) || b.Less(a) || !b.Less(c) || !b.Less(d) || d.Less(b) {
		t.Error("nightlies are not ordered by date, then number (numerically), then version")
	}
}

func TestNewestPicksTheLatestPublishedNightly(t *testing.T) {
	w := newWorld(t)
	w.add("v0.1.7-nightly.20261101.4000")
	w.add("v0.1.7-nightly.20261102.9")
	w.add("v0.1.7-nightly.20261102.10")                                       // the newest: 10 is above 9
	w.add("v0.1.7-nightly.20261103.1", func(r *release) { r.draft = true })   // a draft
	w.add("v0.1.7-nightly.20261104.1", func(r *release) { r.assets = false }) // no SHA256SUMS yet
	w.add("v0.1.8")                                                           // not a nightly
	w.add("v0.1.7-nightly.20269999.1")                                        // not a date
	rel, err := w.client().Newest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v0.1.7-nightly.20261102.10" || rel.Version != "0.1.7-nightly.20261102.10" || !strings.HasSuffix(rel.SumsURL, "/dl/v0.1.7-nightly.20261102.10/SHA256SUMS") {
		t.Errorf("newest: %+v", rel)
	}
	// nothing usable
	empty := newWorld(t)
	empty.add("v0.1.8")
	if _, err := empty.client().Newest(context.Background()); err == nil {
		t.Error("a nightly was invented")
	}
}

func TestFetchGathersAndChecksThePin(t *testing.T) {
	w := newWorld(t)
	tag := "v0.1.9-nightly.20261105.2"
	w.add(tag)
	c := w.client()
	rel, err := c.Newest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := c.Fetch(context.Background(), rel)
	if err != nil {
		t.Fatal(err)
	}
	ver := "0.1.9-nightly.20261105.2"
	if pin.Version != ver || pin.Tag != tag || pin.Integrity != sri(w.tarballs[ver]) || pin.Commit != w.commits[tag] || len(pin.Commit) != 40 {
		t.Errorf("pin: %+v", pin)
	}
	if len(pin.Archives) != len(Platforms) {
		t.Fatalf("archives: %v", pin.Archives)
	}
	for _, p := range Platforms {
		if len(pin.Archives[p]) != 64 {
			t.Errorf("%s: %q", p, pin.Archives[p])
		}
	}
	if _, ok := pin.Archives["win32-arm64"]; ok {
		t.Error("a platform Rubi Home doesn't install on was pinned")
	}
	// the token went to the GitHub API's own paths and nowhere else
	for path, a := range w.auth {
		api := strings.HasPrefix(path, "/repos/")
		if api && a != "Bearer ghp_test_token_not_real" {
			t.Errorf("%s: no token (%q)", path, a)
		}
		if !api && a != "" {
			t.Errorf("%s: the token was sent to a download address: %q", path, a)
		}
	}
}

func TestFetchRefusesWhatDoesNotAddUp(t *testing.T) {
	tag := "v0.1.9-nightly.20261105.2"
	ver := strings.TrimPrefix(tag, "v")
	for name, c := range map[string]struct {
		setup func(w *fakeWorld)
		want  string
	}{
		"a platform is missing": {func(w *fakeWorld) {}, "has no archive for linux-arm64"},
		"sums not a hash":       {func(w *fakeWorld) { w.sums[tag] = "zzzz  t3-" + ver + "-darwin-arm64.tar.gz\n" }, "not a SHA-256"},
		"bad sum":               {func(w *fakeWorld) { w.sums[tag] = "abc123  t3-" + ver + "-darwin-arm64.tar.gz\n" }, "not a SHA-256"},
		"conflicting sums": {func(w *fakeWorld) {
			w.sums[tag] += fmt.Sprintf("%064x  t3-%s-linux-x64.tar.gz\n", 12345, ver)
		}, "twice with different sums"},
		"wrong integrity":  {func(w *fakeWorld) { w.integr[ver] = sri([]byte("something else")) }, "does not match the integrity"},
		"no sha512":        {func(w *fakeWorld) { w.integr[ver] = "sha1-abcdef" }, "no sha512 integrity"},
		"no such version":  {func(w *fakeWorld) { delete(w.tarballs, ver) }, "npm"},
		"registry down":    {func(w *fakeWorld) { w.status["/t3/"] = 503 }, "HTTP 503"},
		"tarball missing":  {func(w *fakeWorld) { w.status["/tarballs/"] = 404 }, "HTTP 404"},
		"sums missing":     {func(w *fakeWorld) { w.status["/dl/"] = 404 }, "HTTP 404"},
		"commit unknown":   {func(w *fakeWorld) { delete(w.commits, tag) }, "commit"},
		"commit not a sha": {func(w *fakeWorld) { w.commits[tag] = "not-a-commit" }, "not what was expected"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			opt := func(r *release) {}
			if name == "a platform is missing" {
				opt = func(r *release) { r.dropPlatform = "linux-arm64" }
			}
			w.add(tag, opt)
			c.setup(w)
			cl := w.client()
			rel, err := cl.Newest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cl.Fetch(context.Background(), rel); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v (want %q)", err, c.want)
			}
		})
	}
}

func TestOnlySafeAddressesAreFetched(t *testing.T) {
	for _, bad := range []string{"http://example.com/x", "ftp://example.com/x", "file:///etc/passwd", "", "https://", "//example.com/x", "http://192.0.2.1/x"} {
		if _, err := checkURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"https://api.github.com/x", "http://127.0.0.1:8080/x", "http://localhost:1/x", "http://[::1]:1/x"} {
		if _, err := checkURL(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	if DefaultAPI != "https://api.github.com" || DefaultNPM != "https://registry.npmjs.org" {
		t.Errorf("the defaults are not the real https addresses: %s %s", DefaultAPI, DefaultNPM)
	}
	// a redirect to a plain-http host elsewhere is refused
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://203.0.113.9/x", http.StatusFound)
	}))
	defer hop.Close()
	c := &Client{API: hop.URL, NPM: hop.URL}
	if _, err := c.get(context.Background(), hop.URL+"/a", 100, false); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("redirect: %v", err)
	}
	// and a body larger than allowed is cut off with an error
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("a", 1000))) }))
	defer big.Close()
	if _, err := c.get(context.Background(), big.URL, 100, false); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("big body: %v", err)
	}
}

func TestUpstreamKeepsItsFormatAndTheNodePin(t *testing.T) {
	root := t.TempDir()
	in := `{
  "package": "t3",
  "version": "0.1.7-nightly.20261102.4093",
  "tag": "v0.1.7-nightly.20261102.4093",
  "commit": "84537c4dd976a89b6ec094f43ae3ef7bee12cb1a",
  "integrity": "sha512-AAAA",
  "license": "MIT, Copyright (c) 2026 T3 Tools Inc.",
  "checked": "2026-11-03",
  "runtime": {
    "note": "a note",
    "sha256": {
      "darwin-arm64": "` + strings.Repeat("a", 64) + `",
      "linux-arm64": "` + strings.Repeat("b", 64) + `",
      "linux-x64": "` + strings.Repeat("c", 64) + `"
    }
  },
  "node": {
    "version": "v26.13.0",
    "note": "SHA-256 from nodejs.org/dist/<version>/SHASUMS256.txt",
    "sha256": {
      "darwin-arm64": "` + strings.Repeat("d", 64) + `"
    }
  }
}
`
	os.MkdirAll(filepath.Join(root, "t3"), 0o755)
	os.WriteFile(filepath.Join(root, "t3", "upstream.json"), []byte(in), 0o644)
	u, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Marshal(u)
	if string(b) != in {
		t.Fatalf("a read and a write change the file:\n%s", b)
	}
	// the repository's own file round-trips too
	real, err := os.ReadFile(filepath.Join("..", "..", "t3", "upstream.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ru Upstream
	if err := json.Unmarshal(real, &ru); err != nil {
		t.Fatal(err)
	}
	if rb, _ := Marshal(ru); string(rb) != string(real) {
		t.Errorf("t3/upstream.json is not in the format the tool writes:\n%s", rb)
	}
	// a bump changes the pin and the date, nothing else
	p := Pin{Version: "9.9.9-nightly.20270101.1", Tag: "v9.9.9-nightly.20270101.1", Commit: strings.Repeat("e", 40), Integrity: "sha512-BBBB",
		Archives: map[string]string{"darwin-arm64": strings.Repeat("1", 64), "linux-arm64": strings.Repeat("2", 64), "linux-x64": strings.Repeat("3", 64)}}
	n := Apply(u, p, "2027-01-02")
	if n.Version != p.Version || n.Tag != p.Tag || n.Commit != p.Commit || n.Integrity != p.Integrity || n.Checked != "2027-01-02" ||
		!reflect.DeepEqual(n.Runtime.SHA256, p.Archives) || n.License != u.License || n.Runtime.Note != u.Runtime.Note || string(n.Node) != string(u.Node) {
		t.Errorf("applied: %+v", n)
	}
	if u.Version != "0.1.7-nightly.20261102.4093" {
		t.Error("Apply changed its input")
	}
}

func TestDiffToolsReportsEachSchemaChangeOfTheToolsInUse(t *testing.T) {
	old := map[string]any{
		"t3_thread_send": map[string]any{"type": "object", "properties": map[string]any{
			"message": map[string]any{"type": "string", "maxLength": float64(100)},
			"mode":    map[string]any{"type": "string", "enum": []any{"auto", "queue"}},
			"gone":    map[string]any{"type": "string"}},
			"required": []any{"threadId"}},
		"t3_thread_read":  map[string]any{"type": "object", "properties": map[string]any{"view": map[string]any{"type": "string"}}},
		"t3_project_list": map[string]any{"type": "object"},
		"not_ours":        map[string]any{"type": "object"},
	}
	cur := map[string]any{
		"t3_thread_send": map[string]any{"type": "object", "properties": map[string]any{
			"message": map[string]any{"type": "string", "maxLength": float64(200)},
			"mode":    map[string]any{"type": "string", "enum": []any{"auto", "queue", "steer"}},
			"extra":   map[string]any{"type": "boolean"}},
			"required": []any{"threadId", "message"}},
		"t3_thread_read":    map[string]any{"type": "object", "properties": map[string]any{"view": map[string]any{"type": "string"}}},
		"t3_project_create": map[string]any{"type": "object"},
		"not_ours":          map[string]any{"type": "array"},
	}
	used := []string{"t3_thread_send", "t3_thread_read", "t3_project_list", "t3_project_create"}
	got := DiffTools(old, cur, used)
	var lines []string
	for _, c := range got {
		lines = append(lines, c.Tool+"|"+c.Path+"|"+c.Kind)
	}
	want := []string{
		"t3_project_create|(the tool)|added", "t3_project_list|(the tool)|removed",
		"t3_thread_send|properties.extra|added", "t3_thread_send|properties.gone|removed", "t3_thread_send|properties.message.maxLength|changed",
		"t3_thread_send|properties.mode.enum|changed", "t3_thread_send|required|changed",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range got {
		if c.Tool == "not_ours" || strings.Contains(c.String(), "not_ours") {
			t.Error("a tool Rubi does not use is reported")
		}
	}
	if same := DiffTools(old, old, used); len(same) != 0 {
		t.Errorf("identical schemas differ: %v", same)
	}
	if !strings.Contains(got[4].String(), "`properties.message.maxLength` changed: 100 -> 200") {
		t.Errorf("text: %s", got[4])
	}
}

// The release archive of a Windows platform is a zip and every other is a tar.gz; Node's Windows archive is "win-x64".
func TestArchiveNamesPerPlatform(t *testing.T) {
	for p, want := range map[string]string{
		"darwin-arm64": "t3-1.2.3-n.20260101.1-darwin-arm64.tar.gz",
		"linux-x64":    "t3-1.2.3-n.20260101.1-linux-x64.tar.gz",
		"win32-x64":    "t3-1.2.3-n.20260101.1-win32-x64.zip",
	} {
		if got := ArchiveName("1.2.3-n.20260101.1", p); got != want {
			t.Errorf("%s: %s", p, got)
		}
	}
	for p, want := range map[string]string{
		"darwin-arm64": "node-v26.13.0-darwin-arm64.tar.gz",
		"linux-arm64":  "node-v26.13.0-linux-arm64.tar.gz",
		"win32-x64":    "node-v26.13.0-win-x64.zip",
	} {
		if got := NodeArchive("v26.13.0", p); got != want {
			t.Errorf("%s: %s", p, got)
		}
	}
	for p := range NoBuild {
		for _, q := range Platforms {
			if p == q {
				t.Errorf("%s is both pinned and listed as having no build", p)
			}
		}
	}
}

// A Windows platform's sum is that of the .zip; a tar.gz of that name is not it, and a missing zip fails the bump.
func TestParseSumsTakesTheZipForWindows(t *testing.T) {
	ver := "0.1.9-nightly.20261105.2"
	all := func(skipWin bool) string {
		var sb strings.Builder
		for i, p := range Platforms {
			if skipWin && p == "win32-x64" {
				continue
			}
			fmt.Fprintf(&sb, "%064x  %s\n", i+1, ArchiveName(ver, p))
		}
		return sb.String()
	}
	got, err := ParseSums(all(false), ver)
	if err != nil || got["win32-x64"] != fmt.Sprintf("%064x", 4) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseSums(all(true), ver); err == nil || !strings.Contains(err.Error(), "win32-x64") {
		t.Errorf("no zip: %v", err)
	}
	if _, err := ParseSums(all(true)+fmt.Sprintf("%064x  t3-%s-win32-x64.tar.gz\n", 9, ver), ver); err == nil {
		t.Error("a tar.gz was taken for the Windows archive")
	}
}

// The day upstream publishes a runtime for a platform that has none, the bump says so.
func TestNowPublishedFindsTheDayUpstreamAddsAPlatform(t *testing.T) {
	ver := "0.1.9-nightly.20261105.2"
	if got := NowPublished("aa  t3-"+ver+"-linux-x64.tar.gz\n", ver); len(got) != 0 {
		t.Errorf("%v", got)
	}
	if got := NowPublished("aa  t3-"+ver+"-darwin-x64.tar.gz\nbb  t3-"+ver+"-linux-x64.tar.gz\n", ver); len(got) != 1 || got[0] != "darwin-x64" {
		t.Errorf("%v", got)
	}
	if got := NowPublished("aa  *t3-"+ver+"-darwin-x64.tar.gz\n", ver); len(got) != 1 {
		t.Errorf("binary-mode marker: %v", got)
	}
	r := Report(Result{Changed: true, Pin: Pin{Published: []string{"darwin-x64"}}, Old: Upstream{Version: "1"}, New: Upstream{Tag: "v2", Version: "2"}, Install: "skipped", Contract: "skipped"})
	if !strings.Contains(r, "Upstream now publishes T3 for darwin-x64") {
		t.Errorf("the report does not say so:\n%s", r)
	}
}

// Node is not bumped, so a platform added to Platforms needs its Node sum by hand: the real upstream.json has them all, and a
// missing or malformed one stops the bump.
func TestCheckNodeNeedsEveryPlatform(t *testing.T) {
	u, err := Read(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckNode(u); err != nil {
		t.Fatalf("the real pin: %v", err)
	}
	for _, p := range Platforms {
		if len(u.Runtime.SHA256[p]) != 64 {
			t.Errorf("the real pin has no T3 archive sum for %s", p)
		}
	}
	for name, raw := range map[string]string{
		"no node":      ``,
		"no win32-x64": `{"version":"v1","sha256":{"darwin-arm64":"` + strings.Repeat("a", 64) + `","linux-arm64":"` + strings.Repeat("a", 64) + `","linux-x64":"` + strings.Repeat("a", 64) + `"}}`,
		"not a sum":    `{"version":"v1","sha256":{"darwin-arm64":"zz","linux-arm64":"` + strings.Repeat("a", 64) + `","linux-x64":"` + strings.Repeat("a", 64) + `","win32-x64":"` + strings.Repeat("a", 64) + `"}}`,
		"no version":   `{"sha256":{}}`,
	} {
		bad := u
		bad.Node = json.RawMessage(raw)
		if err := CheckNode(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
