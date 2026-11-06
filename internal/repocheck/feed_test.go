package repocheck

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseFeedNamesTheMacAppOnlyWhenTheReleaseHasIt runs deploy/pages/latest.jq (what pages.yml runs) on made-up
// GitHub release lists (skipped without jq).
func TestReleaseFeedNamesTheMacAppOnlyWhenTheReleaseHasIt(t *testing.T) {
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("jq is not installed")
	}
	pages, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", "pages.yml"))
	if err != nil || !strings.Contains(string(pages), "jq -f deploy/pages/latest.jq") {
		t.Fatalf("pages.yml does not run deploy/pages/latest.jq (%v)", err)
	}
	feed := func(releases string) map[string]any {
		cmd := exec.Command(jq, "-f", filepath.Join(root(t), "deploy", "pages", "latest.jq"))
		cmd.Stdin = strings.NewReader(releases)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("jq: %v\n%s", err, out)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return m
	}
	rel := func(tag, at string, draft bool, assets ...string) string {
		var as []string
		for _, a := range assets {
			as = append(as, `{"name":"`+a+`"}`)
		}
		d := "false"
		if draft {
			d = "true"
		}
		return `{"tag_name":"` + tag + `","published_at":"` + at + `","html_url":"https://example/` + tag + `","draft":` + d + `,"assets":[` + strings.Join(as, ",") + `]}`
	}
	list := func(rs ...string) string { return "[" + strings.Join(rs, ",") + "]" }

	got := feed(list(rel("v1.0.0", "2026-01-01", false, "rubi-home-v1.0.0-macos-universal.zip", "SHA256SUMS"), rel("v0.9.0", "2025-01-01", false)))
	if got["version"] != "v1.0.0" || got["mac_app"] != "rubi-home-v1.0.0-macos-universal.zip" || got["notes_url"] != "https://example/v1.0.0" {
		t.Errorf("newest with the app: %v", got)
	}
	got = feed(list(rel("v1.0.0", "2026-01-01", false, "SHA256SUMS", "rubi-home-v1.0.0-darwin-arm64.tar.gz")))
	if _, has := got["mac_app"]; has || got["version"] != "v1.0.0" {
		t.Errorf("a release without the app must carry no mac_app: %v", got)
	}
	// The app of an older release is not the newest release's app; a draft is never the newest.
	got = feed(list(rel("v1.0.0", "2026-01-01", false), rel("v0.9.0", "2025-01-01", false, "rubi-home-v0.9.0-macos-universal.zip"), rel("v2.0.0", "2027-01-01", true, "rubi-home-v2.0.0-macos-universal.zip")))
	if _, has := got["mac_app"]; has || got["version"] != "v1.0.0" {
		t.Errorf("older release's or draft's app: %v", got)
	}
	// An asset of another name (a tag that is only a prefix, another arch) does not count.
	got = feed(list(rel("v1.0.0", "2026-01-01", false, "rubi-home-v1.0.0-macos-arm64.zip", "rubi-home-v1.0.0-macos-universal.zip.sig")))
	if _, has := got["mac_app"]; has {
		t.Errorf("a wrong name counted: %v", got)
	}
}
