package plugins

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="

func manifest(t *testing.T, patch string) Manifest {
	t.Helper()
	base := `{"schema":1,"id":"demo","name":"Demo","version":"v1.0.0","api":1,"entry":"demo",
		"publisher":{"name":"x","key":"` + testKey + `"},
		"actions":[{"kind":"demo.send","title":"Send","default_level":"strong"}],
		"tools":[{"name":"demo_send","description":"d","input_schema":{"type":"object"}}]}`
	var m map[string]any
	_ = json.Unmarshal([]byte(base), &m)
	if patch != "" {
		var p map[string]any
		if err := json.Unmarshal([]byte(patch), &p); err != nil {
			t.Fatal(err)
		}
		for k, v := range p {
			m[k] = v
		}
	}
	b, _ := json.Marshal(m)
	var out Manifest
	_ = json.Unmarshal(b, &out)
	return out
}

func TestCheck(t *testing.T) {
	ok := manifest(t, "")
	if err := Check(&ok); err != nil {
		t.Fatal(err)
	}
	home := manifest(t, `{"home":["hue","shortcuts","claude","claude-sessions","claude-start"]}`)
	if err := Check(&home); err != nil {
		t.Fatalf("Rubi Home capabilities: %v", err)
	}
	// claude-agent and agent-setup are the official plugin's: its id and its publisher key.
	official := manifest(t, `{"id":"`+OfficialPluginID+`","actions":[],"tools":[],"home":["claude-agent","agent-setup"],"publisher":{"name":"x","key":"`+OfficialPublisherKey+`"}}`)
	if err := Check(&official); err != nil {
		t.Fatalf("the official plugin: %v", err)
	}
	for name, patch := range map[string]string{
		"unknown home capability":  `{"home":["claude-desktop"]}`,
		"third party claude-agent": `{"home":["claude-agent"]}`,
		"third party agent-setup":  `{"home":["agent-setup"]}`,
		"imitation id, own key":    `{"id":"` + OfficialPluginID + `","actions":[],"tools":[],"home":["claude-agent"],"publisher":{"name":"x","key":"MCowBQYDK2VwAyEAkG4bjWz38ru0Wpsihr0VVFTAMPjRvK8darUtybopFdk="}}`,
		"official key, other id":   `{"home":["agent-setup"],"publisher":{"name":"x","key":"` + OfficialPublisherKey + `"}}`,
		"reserved id":              `{"id":"rubi-core"}`,
		"bad id":                   `{"id":"Demo!"}`,
		"foreign action":           `{"actions":[{"kind":"other.send","title":"Send","default_level":"none"}]}`,
		"core action":              `{"actions":[{"kind":"rubi.settings","title":"x","default_level":"none"}]}`,
		"foreign tool":             `{"tools":[{"name":"rubi_lock","description":"d","input_schema":{"type":"object"}}]}`,
		"bad level":                `{"actions":[{"kind":"demo.x","title":"x","default_level":"maybe"}]}`,
		"entry escapes":            `{"entry":"../../bin/sh"}`,
		"absolute entry":           `{"entry":"/bin/sh"}`,
		"unknown api":              `{"api":2}`,
		"bad key":                  `{"publisher":{"name":"x","key":"aGVsbG8="}}`,
		"bad version":              `{"version":"latest"}`,
		"tool without schema":      `{"tools":[{"name":"demo_x","description":"d","input_schema":{"type":"string"}}]}`,
		"duplicate secret key":     `{"secrets":[{"key":"a","label":"A"},{"key":"a","label":"B"}]}`,
	} {
		m := manifest(t, patch)
		if err := Check(&m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReleaseBase(t *testing.T) {
	for _, c := range [][3]string{
		{"https://github.com/o/r", "", "https://github.com/o/r/releases/latest/download/"},
		{"https://github.com/o/r.git/", "v1.2.3", "https://github.com/o/r/releases/download/v1.2.3/"},
		{"https://example.com/plugins/x", "", "https://example.com/plugins/x/latest/"},
		{"https://example.com/plugins/x/", "v1.0.0", "https://example.com/plugins/x/v1.0.0/"},
	} {
		got, err := ReleaseBase(c[0], c[1])
		if err != nil || got != c[2] {
			t.Errorf("ReleaseBase(%q, %q) = %q, %v; want %q", c[0], c[1], got, err, c[2])
		}
	}
	for _, bad := range []string{"http://example.com/x", "ftp://example.com/x", "not a url"} {
		if _, err := ReleaseBase(bad, ""); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := ReleaseBase("https://example.com/x", "../../etc"); err == nil {
		t.Error("accepted a path as version")
	}
}

func writeTar(t *testing.T, hdrs []*tar.Header) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, h := range hdrs {
		_ = tw.WriteHeader(h)
		if h.Size > 0 {
			_, _ = tw.Write([]byte(strings.Repeat("x", int(h.Size))))
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestExtractRefusesUnsafeEntries(t *testing.T) {
	for name, h := range map[string]*tar.Header{
		"parent dir": {Name: "../evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		"absolute":   {Name: "/etc/evil", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		"symlink":    {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		"manifest":   {Name: "rubi-plugin.json", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
	} {
		if err := extract(writeTar(t, []*tar.Header{h}), t.TempDir()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	if err := extract(writeTar(t, []*tar.Header{{Name: "bin/demo", Typeflag: tar.TypeReg, Size: 2, Mode: 0o755}}), dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "bin", "demo"))
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("extracted file: %v %v", st, err)
	}
}

func TestTreeHash(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	h1, _ := TreeHash(dir)
	_ = os.Chmod(filepath.Join(dir, "a"), 0o755)
	h2, _ := TreeHash(dir)
	_ = os.WriteFile(filepath.Join(dir, "b"), []byte(""), 0o644)
	h3, _ := TreeHash(dir)
	if h1 == h2 || h2 == h3 {
		t.Fatal("tree hash ignores mode or new files")
	}
	_ = os.Symlink("/etc/passwd", filepath.Join(dir, "c"))
	if _, err := TreeHash(dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

// Permissions and approval levels come from the verified package, never from the editable index.
func TestTrustUsesTheVerifiedManifest(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := manifest(t, "")
	dir := s.Dir("demo", "v1.0.0")
	_ = os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(real)
	_ = os.WriteFile(filepath.Join(dir, "rubi-plugin.json"), b, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "demo"), []byte("#!/bin/sh\n"), 0o755)
	tree, err := TreeHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Someone edits the index: the send action no longer asks, and an unapproved plugin appears.
	forged := manifest(t, `{"actions":[{"kind":"demo.send","title":"Send","default_level":"none","locked":true}],"home":["shortcuts"]}`)
	s.index["demo"] = forged
	s.index["evil"] = manifest(t, `{"id":"evil"}`)

	s.Retain(map[string]bool{"demo": true})
	m, err := s.Trust("demo", "v1.0.0", tree)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("demo")
	if m.Actions[0].DefaultLevel != "strong" || got.Actions[0].DefaultLevel != "strong" || len(got.Home) != 0 {
		t.Fatalf("the edited index is still in use: %+v", got.Actions)
	}
	if _, ok := s.Get("evil"); ok {
		t.Fatal("a plugin the user never approved is still listed")
	}
	// Changed files are refused.
	_ = os.WriteFile(filepath.Join(dir, "demo"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	if _, err := s.Trust("demo", "v1.0.0", tree); err == nil {
		t.Fatal("modified files trusted")
	}
}

// The refusal of a reserved capability is for that reason, not another one in the manifest.
func TestReservedHomeCapabilityReason(t *testing.T) {
	m := manifest(t, `{"home":["claude-agent"]}`)
	if err := Check(&m); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("%v", err)
	}
	m = manifest(t, `{"id":"`+OfficialPluginID+`","actions":[],"tools":[],"home":["claude-agent"],"publisher":{"name":"x","key":"MCowBQYDK2VwAyEAkG4bjWz38ru0Wpsihr0VVFTAMPjRvK8darUtybopFdk="}}`)
	if err := Check(&m); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("%v", err)
	}
}
