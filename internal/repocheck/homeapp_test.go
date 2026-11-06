package repocheck

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildApp runs scripts/build-home-app.sh with a fake binary and returns its output and the zip's path.
func buildApp(t *testing.T, version, arch, binary, outdir string) (string, string, error) {
	t.Helper()
	for _, tool := range []string{"sh", "zip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	if binary == "" {
		binary = filepath.Join(t.TempDir(), "rubi-home")
		if err := os.WriteFile(binary, []byte("#!fake rubi-home\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), version, arch, binary, outdir).CombinedOutput()
	return string(out), filepath.Join(outdir, "rubi-home-"+version+"-macos-"+arch+".zip"), err
}

// plistValues reads the flat <key>/<value> pairs of the Info.plist's one dict; true and false come back as such.
func plistValues(t *testing.T, b []byte) map[string]string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(b))
	vals := map[string]string{}
	var key, text string
	var in bool // inside a <string>/<key>
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return vals
		}
		if err != nil {
			t.Fatalf("Info.plist is not XML: %v", err)
		}
		switch e := tok.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "key", "string":
				in, text = true, ""
			case "true", "false":
				vals[key] = e.Name.Local
			}
		case xml.CharData:
			if in {
				text += string(e)
			}
		case xml.EndElement:
			switch e.Name.Local {
			case "key":
				key, in = text, false
			case "string":
				vals[key], in = text, false
			}
		}
	}
}

func TestHomeAppZipHasTheBundleLayout(t *testing.T) {
	outdir := t.TempDir()
	if out, zipPath, err := buildApp(t, "v1.2.3-rc.4", "arm64", "", outdir); err != nil {
		t.Fatalf("%v\n%s", err, out)
	} else {
		r, err := zip.OpenReader(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		files := map[string]*zip.File{}
		var names []string
		for _, f := range r.File {
			files[f.Name] = f
			names = append(names, f.Name)
		}
		want := []string{"Rubi Home.app/Contents/Info.plist", "Rubi Home.app/Contents/MacOS/rubi-home"}
		if strings.Join(names, "|") != strings.Join(want, "|") {
			t.Fatalf("zip holds %q, want %q (sorted, nothing else)", names, want)
		}
		bin := files[want[1]]
		if bin.Mode()&0o111 != 0o111 {
			t.Fatalf("the binary is %v in the zip: it must stay executable", bin.Mode())
		}
		if files[want[0]].Mode()&0o111 != 0 {
			t.Fatalf("Info.plist is executable: %v", files[want[0]].Mode())
		}
		for _, f := range r.File {
			if y := f.Modified.Year(); y != 2000 {
				t.Fatalf("%s has time %v: times must be fixed", f.Name, f.Modified)
			}
		}
		rc, _ := bin.Open()
		got, _ := io.ReadAll(rc)
		rc.Close()
		if string(got) != "#!fake rubi-home\n" {
			t.Fatalf("the binary in the app is not the one given: %q", got)
		}

		rc, _ = files[want[0]].Open()
		plist, _ := io.ReadAll(rc)
		rc.Close()
		v := plistValues(t, plist)
		for k, want := range map[string]string{
			"CFBundleIdentifier":         "com.rubi-project.home",
			"CFBundleName":               "Rubi Home",
			"CFBundleDisplayName":        "Rubi Home",
			"CFBundleExecutable":         "rubi-home",
			"CFBundlePackageType":        "APPL",
			"CFBundleShortVersionString": "1.2.3-rc.4",
			"CFBundleVersion":            "1.2.3-rc.4",
			"LSMinimumSystemVersion":     "13.0",
			"LSUIElement":                "true",
		} {
			if v[k] != want {
				t.Errorf("Info.plist %s = %q, want %q", k, v[k], want)
			}
		}
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("/usr/bin/plutil"); err == nil {
				p := filepath.Join(t.TempDir(), "Info.plist")
				if err := os.WriteFile(p, plist, 0o600); err != nil {
					t.Fatal(err)
				}
				if out, err := exec.Command("/usr/bin/plutil", "-lint", p).CombinedOutput(); err != nil {
					t.Fatalf("plutil -lint: %v\n%s", err, out)
				}
				out, err := exec.Command("/usr/bin/plutil", "-extract", "LSUIElement", "raw", "-o", "-", p).Output()
				if err != nil || strings.TrimSpace(string(out)) != "true" {
					t.Fatalf("plutil reads LSUIElement as %q (%v)", out, err)
				}
			}
		}
	}
}

func TestHomeAppZipIsReproducible(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rubi-home")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sums [][]byte
	for i := 0; i < 2; i++ {
		out, zipPath, err := buildApp(t, "v0.15.0", "universal", bin, t.TempDir())
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		b, err := os.ReadFile(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		sums = append(sums, b)
		// The second build runs under another zone, another umask and a newer binary file: none may change the bytes.
		t.Setenv("TZ", "Asia/Tokyo")
		old := syscall.Umask(0o077)
		t.Cleanup(func() { syscall.Umask(old) })
		later := time.Now().Add(time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(bin, later, later); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(sums[0], sums[1]) {
		t.Fatal("building twice gives different bytes")
	}
}

func TestHomeAppBuildRefusesWhatGoesIntoFileNames(t *testing.T) {
	bad := []struct{ version, arch string }{
		{"0.15.0", "arm64"},          // no v
		{"v0.15", "arm64"},           // not a release tag
		{"v0.15.0/../x", "arm64"},    // path
		{"v0.15.0 x", "arm64"},       // space
		{"v0.15.0$(id)", "arm64"},    // shell
		{"v0.15.0<x>", "arm64"},      // would break the plist
		{"v0.15.0\nv1.0.0", "arm64"}, // two lines pass a line-based grep
		{"v0.15.0", "x86_64"},
		{"v0.15.0", "arm64/../x"},
		{"v0.15.0", ""},
		{"v0.15.0", "ARM64"},
	}
	for _, c := range bad {
		outdir := t.TempDir()
		out, _, err := buildApp(t, c.version, c.arch, "", outdir)
		if err == nil {
			t.Errorf("version %q arch %q was accepted:\n%s", c.version, c.arch, out)
		}
		if es, _ := os.ReadDir(outdir); len(es) != 0 {
			t.Errorf("version %q arch %q left files behind: %v", c.version, c.arch, es)
		}
	}
}

func TestHomeAppBuildRefusesAMissingBinary(t *testing.T) {
	if _, _, err := buildApp(t, "v0.15.0", "arm64", filepath.Join(t.TempDir(), "nothing"), t.TempDir()); err == nil {
		t.Fatal("a missing binary was accepted")
	}
}

// TestReleaseBuildsTheUniversalMacAppBeforeTheSums is a lint of release.yml's text (nothing runs a runner here):
// the mac-app job builds both CPUs, joins them with lipo and runs the script, sealed, for "universal"; the release
// job downloads that zip into dist before SHA256SUMS lists it; the zip is attested and published; the tar.gz stay
// for the script installer and Linux. Run `ruby -ryaml -e 'YAML.load_file(ARGV[0])' FILE` for the syntax.
func TestReleaseBuildsTheUniversalMacAppBeforeTheSums(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	job := strings.Index(s, "\n  mac-app:\n")
	if job < 0 || !strings.Contains(s[job:], "runs-on: macos-latest") {
		t.Fatal("no mac-app job on a macOS runner")
	}
	rel := strings.Index(s, "\n  release:\n")
	if rel < job {
		t.Fatal("the release job should follow the mac-app job")
	}
	mac := s[job:rel]
	loop := strings.Index(mac, "for arch in arm64 amd64; do")
	lipo := strings.Index(mac, "lipo -create -output work/home-darwin-universal/rubi-home work/home-darwin-arm64/rubi-home work/home-darwin-amd64/rubi-home")
	call := strings.Index(mac, `RUBI_APP_SIGN=adhoc sh scripts/build-home-app.sh "$GITHUB_REF_NAME" universal work/home-darwin-universal/rubi-home app`)
	if loop < 0 || lipo < loop || call < lipo {
		t.Fatal("the mac-app job must build arm64 and amd64, lipo them, then build the sealed universal app, in that order")
	}
	if !strings.Contains(s[rel:rel+200], "needs: [mac-app, windows-check, microvm]") {
		t.Fatal("the release job does not wait for the mac-app and Windows jobs")
	}
	dl := strings.Index(s[rel:], "actions/download-artifact")
	sums := strings.Index(s, "sha256sum rubi-*.tar.gz rubi-*.zip")
	if dl < 0 || sums < 0 || rel+dl > sums {
		t.Fatal("the app zip must be downloaded into dist before SHA256SUMS lists it")
	}
	if !strings.Contains(s[rel+dl:rel+dl+120], "path: dist") {
		t.Fatal("the app zip is not downloaded into dist")
	}
	if !strings.Contains(s, "ls dist/rubi-home-*-macos-universal.zip") {
		t.Fatal("the release does not check that the universal zip arrived")
	}
	if !strings.Contains(s, `rubi-home-${GITHUB_REF_NAME}-$os-$arch.tar.gz`) {
		t.Fatal("the Home tar.gz must stay: the script installer and Linux use it")
	}
	if !strings.Contains(s, "gh release create") || !strings.Contains(s[strings.Index(s, "gh release create"):], "dist/rubi-*.zip") {
		t.Fatal("the zip is not published")
	}
	if !strings.Contains(s, `subject-path: "dist/rubi-*.tar.gz, dist/rubi-*.zip`) {
		t.Fatal("the zip is not attested")
	}
	// The owner decided against a Developer ID: the workflow says so instead of keeping a TODO.
	if strings.Contains(s, "TODO(macOS signing)") || strings.Contains(s, "MACOS_SIGN_IDENTITY") {
		t.Fatal("the release workflow still presents Developer ID signing as open")
	}
	if !strings.Contains(s, "does not plan a Developer ID signature") {
		t.Fatal("the release workflow does not state the signing decision")
	}
	// The name the script makes is what the globs, the feed filter and the panel's link expect.
	sh, err := os.ReadFile(filepath.Join(root(t), "scripts", "build-home-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sh), `zipname="rubi-home-$version-macos-$arch.zip"`) {
		t.Fatal("the zip's name changed: update release.yml's globs, deploy/pages/latest.jq and the panel's download link")
	}
	jq, err := os.ReadFile(filepath.Join(root(t), "deploy", "pages", "latest.jq"))
	if err != nil || !strings.Contains(string(jq), `"-macos-universal.zip"`) {
		t.Fatalf("the feed filter does not name the universal zip (%v)", err)
	}
}

// TestPanelDownloadLinkNamesTheFileTheBuildMakes runs the panel's link helper under node and compares its address
// with the file scripts/build-home-app.sh really makes, and with the repository the installer downloads from. A
// feed that does not say the release has the app gets no file link.
func TestPanelDownloadLinkNamesTheFileTheBuildMakes(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	src, err := os.ReadFile(filepath.Join(root(t), "panel/js/homecode.js"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := filepath.Join(dir, "homecode.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import * as m from ` + fileURLJS(mod) + `;
const ok = { version: "v1.2.3-rc.4", mac_app: "rubi-home-v1.2.3-rc.4-macos-universal.zip" };
console.log(JSON.stringify({
  link: m.homeMacAppURL(ok),
  bad: [
    m.homeMacAppURL({ version: "v1.2.3-rc.4" }),                                            // an older feed: no mac_app
    m.homeMacAppURL({ version: "v1.2.3-rc.4", mac_app: null }),
    m.homeMacAppURL({ version: "v1.2.3-rc.4", mac_app: "rubi-home-v1.2.3-rc.4-macos-arm64.zip" }),
    m.homeMacAppURL({ version: "v1.2.3-rc.4", mac_app: "rubi-home-v1.2.2-macos-universal.zip" }), // another release's file
    m.homeMacAppURL({ version: "v1.2.3/../x", mac_app: "rubi-home-v1.2.3/../x-macos-universal.zip" }),
    m.homeMacAppURL({ version: "1.2.3", mac_app: "rubi-home-1.2.3-macos-universal.zip" }),
    m.homeMacAppURL({ version: "v1.2.3\nv2.0.0", mac_app: "rubi-home-v1.2.3\nv2.0.0-macos-universal.zip" }),
    m.homeMacAppURL(null), m.homeMacAppURL(undefined), m.homeMacAppURL("x"),
  ],
  feed: [m.homeFeedVersion({ version: "v0.15.0" }), m.homeFeedVersion({ version: "v0.15.0/../x" }), m.homeFeedVersion({}), m.homeFeedVersion(null), m.homeFeedVersion({ version: 5 })],
}));`
	cmd := exec.Command(node, "--input-type=module", "-e", script)
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got struct {
		Link string
		Bad  []string
		Feed []string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	install, err := os.ReadFile(filepath.Join(root(t), "panel", "install-home.sh"))
	if err != nil {
		t.Fatal(err)
	}
	repo := regexp.MustCompile(`REPO="([^"]+)"`).FindSubmatch(install)
	if repo == nil {
		t.Fatal("install-home.sh names no repository")
	}
	_, zipPath, err := buildApp(t, "v1.2.3-rc.4", "universal", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://github.com/" + string(repo[1]) + "/releases/download/v1.2.3-rc.4/" + filepath.Base(zipPath); got.Link != want {
		t.Errorf("the panel links %q, the build makes %q", got.Link, want)
	}
	for i, b := range got.Bad {
		if b != "" {
			t.Errorf("bad feed %d gave a link: %q", i, b)
		}
	}
	if got.Feed[0] != "v0.15.0" || got.Feed[1] != "" || got.Feed[2] != "" || got.Feed[3] != "" || got.Feed[4] != "" {
		t.Errorf("feed versions %q", got.Feed)
	}
}

// TestHomeScreenOffersTheBotFirstThenTheDownload reads the panel source: the Bot message first, the Mac app folded
// under it for people with no Bot, and the order when a code is waiting (pair first, install steps folded under
// "Not installed yet?").
func TestHomeScreenOffersTheBotFirstThenTheDownload(t *testing.T) {
	s := readRepo(t, "panel/js/app.js")
	for _, want := range []string{"Download Rubi Home for Mac", "Open Anyway", "Privacy & Security", "Open the Rubi panel", "HOME_BOT_MESSAGE", `"Not installed yet?"`, "No Bot on that Mac?"} {
		if !strings.Contains(s, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(s, "Intel Mac\"") || strings.Contains(s, "macos-arm64.zip") {
		t.Error("the panel offers a per-CPU download: one universal app is the only link")
	}
	i := strings.Index(s, "function homeDeviceScreen")
	if i < 0 {
		t.Fatal("no homeDeviceScreen")
	}
	body := s[i:]
	pair, fold := strings.Index(body, "...pairBlock,"), strings.Index(body, `"Not installed yet?"`)
	plain := strings.Index(body, "[...install, ...pairBlock]")
	if pair < 0 || fold < 0 || pair > fold {
		t.Error("with a code waiting, the pairing block must come before the folded install steps")
	}
	if plain < 0 {
		t.Error("without a waiting code, the install steps must come first")
	}
	// the Bot message comes before the Mac download, and the download is folded
	steps := jsFunc(t, s, "homeInstallSteps")
	msg, dl := strings.Index(steps, "HOME_BOT_MESSAGE"), strings.Index(steps, `h("details"`)
	if msg < 0 || dl < 0 || msg > dl || !strings.Contains(steps[dl:], "home-dl") {
		t.Error("the Bot message must come first and the Mac download must sit in a fold under it")
	}
}

// TestHomeScreenNeverShowsACommandToType: the user is never shown a command on the "Add a home computer" screen
// (project rule 2). The installer line is the Bot's, in install.md; the panel holds none of it.
func TestHomeScreenNeverShowsACommandToType(t *testing.T) {
	s := readRepo(t, "panel/js/app.js")
	for _, f := range []string{"homeInstallSteps", "homeDeviceScreen"} {
		body := jsFunc(t, s, f)
		for _, bad := range []string{"curl", "install-home.sh", "| sh", "terminal", "Terminal", "HOME_INSTALL", `class: "cmd"`, "sudo"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s shows %q: the user would be told to type a command", f, bad)
			}
		}
	}
	if strings.Contains(s, "HOME_INSTALL") || strings.Contains(s, "install-home.sh") {
		t.Error("the panel still names the installer script")
	}
}

// TestHomeBotMessageIsWhatInstallMdServes: the message the panel gives the user is the one the Bot's page answers.
// install.md is the Bot skill (pages.yml publishes it), and its Home section is what the Bot follows.
func TestHomeBotMessageIsWhatInstallMdServes(t *testing.T) {
	s := readRepo(t, "panel/js/app.js")
	m := regexp.MustCompile(`const HOME_BOT_MESSAGE = "([^"]+)";`).FindStringSubmatch(s)
	if m == nil {
		t.Fatal("no HOME_BOT_MESSAGE in app.js")
	}
	if m[1] != "Install Rubi Home on this computer by following https://rubi-panel.com/install.md" {
		t.Errorf("the Bot message is %q", m[1])
	}
	for _, bad := range []string{"curl", "|", "sudo", "`"} {
		if strings.Contains(m[1], bad) {
			t.Errorf("the Bot message holds %q: it is a sentence and a link", bad)
		}
	}
	if !strings.Contains(readRepo(t, ".github/workflows/pages.yml"), "cp skills/grok-bot/rubi-project/SKILL.md panel/install.md") {
		t.Fatal("pages.yml does not publish the Bot skill as install.md")
	}
	skill := readRepo(t, "skills/grok-bot/rubi-project/SKILL.md")
	i := strings.Index(skill, "## Install Rubi Home on the user's computer")
	if i < 0 {
		t.Fatal("install.md has no section for installing Rubi Home")
	}
	sec := skill[i:]
	if j := strings.Index(sec[3:], "\n## "); j >= 0 {
		sec = sec[:j+3]
	}
	for _, want := range []string{m[1], "https://rubi-panel.com/install-home.sh", "~/.rubi-home/bin/rubi-home open", "macOS", "Linux", "Windows",
		"https://rubi-panel.com/install-home.ps1", "-ExecutionPolicy Bypass -File", "Windows 10 or later", "not available on this computer yet"} {
		if !strings.Contains(sec, want) {
			t.Errorf("the Home section of install.md lacks %q", want)
		}
	}
	if strings.Contains(sec, "Windows is not supported yet") {
		t.Error("install.md still says Windows is not supported")
	}
	// the scripts the Bot is told to run are the ones the Pages deploy serves
	for _, f := range []string{"install-home.sh", "install-home.ps1"} {
		if _, err := os.Stat(filepath.Join(root(t), "panel", f)); err != nil {
			t.Errorf("install.md names %s, but panel/%s is missing: %v", f, f, err)
		}
	}
}

// The release seals the app with an ad-hoc signature (RUBI_APP_SIGN=adhoc, on a macOS runner): an unsealed
// bundle fails `codesign --verify` ("code has no resources but signature indicates they must be present"),
// and macOS may then call a downloaded app damaged instead of offering "Open Anyway".
func TestHomeAppSealedBuildVerifies(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign is macOS only")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("codesign is not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "rubi-home")
	build := exec.Command("go", "build", "-o", bin, src)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building a stand-in binary: %v\n%s", err, out)
	}
	sealed := func(out string) string {
		cmd := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), "v1.2.3", "arm64", bin, out)
		cmd.Env = append(os.Environ(), "RUBI_APP_SIGN=adhoc")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sealed build: %v\n%s", err, b)
		}
		return filepath.Join(out, "rubi-home-v1.2.3-macos-arm64.zip")
	}
	z1, z2 := sealed(filepath.Join(dir, "a")), sealed(filepath.Join(dir, "b"))
	b1, _ := os.ReadFile(z1)
	b2, _ := os.ReadFile(z2)
	if !bytes.Equal(b1, b2) {
		t.Error("two sealed builds differ")
	}
	unzipped := filepath.Join(dir, "x")
	if out, err := exec.Command("/usr/bin/ditto", "-x", "-k", z1, unzipped).CombinedOutput(); err != nil {
		t.Fatalf("unzip: %v\n%s", err, out)
	}
	app := filepath.Join(unzipped, "Rubi Home.app")
	if out, err := exec.Command("codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("the sealed app does not verify after unzipping: %v\n%s", err, out)
	}

	// Anything but adhoc is refused rather than silently building an unsealed app.
	cmd := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), "v1.2.3", "arm64", bin, filepath.Join(dir, "c"))
	cmd.Env = append(os.Environ(), "RUBI_APP_SIGN=yes")
	if err := cmd.Run(); err == nil {
		t.Error("RUBI_APP_SIGN=yes was accepted")
	}
}

// TestHomeAppUniversalBuildBundlesAndVerifies makes tiny arm64 and amd64 programs, joins them with lipo as the
// release does, builds the sealed "universal" app from the result, and checks that both slices are in the unzipped app
// and that the app verifies (macOS only).
func TestHomeAppUniversalBuildBundlesAndVerifies(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("lipo and codesign are macOS only")
	}
	for _, tool := range []string{"lipo", "codesign"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var slices []string
	for _, arch := range []string{"arm64", "amd64"} {
		bin := filepath.Join(dir, "rubi-home-"+arch)
		build := exec.Command("go", "build", "-o", bin, src)
		build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=darwin", "GOARCH="+arch, "GO111MODULE=off")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building a %s stand-in: %v\n%s", arch, err, out)
		}
		slices = append(slices, bin)
	}
	fat := filepath.Join(dir, "rubi-home")
	if out, err := exec.Command("lipo", append([]string{"-create", "-output", fat}, slices...)...).CombinedOutput(); err != nil {
		t.Fatalf("lipo: %v\n%s", err, out)
	}
	out := filepath.Join(dir, "out")
	cmd := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), "v1.2.3", "universal", fat, out)
	cmd.Env = append(os.Environ(), "RUBI_APP_SIGN=adhoc")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("universal build: %v\n%s", err, b)
	}
	unzipped := filepath.Join(dir, "x")
	if b, err := exec.Command("/usr/bin/ditto", "-x", "-k", filepath.Join(out, "rubi-home-v1.2.3-macos-universal.zip"), unzipped).CombinedOutput(); err != nil {
		t.Fatalf("unzip: %v\n%s", err, b)
	}
	app := filepath.Join(unzipped, "Rubi Home.app")
	if b, err := exec.Command("codesign", "--verify", "--strict", app).CombinedOutput(); err != nil {
		t.Fatalf("the universal app does not verify: %v\n%s", err, b)
	}
	archs, err := exec.Command("lipo", "-archs", filepath.Join(app, "Contents", "MacOS", "rubi-home")).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(archs))
	if len(got) != 2 || !(got[0] == "arm64" && got[1] == "x86_64" || got[0] == "x86_64" && got[1] == "arm64") {
		t.Fatalf("the app holds %q, want arm64 and x86_64", got)
	}
}

// fakeTool puts a shell script named name first on PATH and returns the environment to run the build with.
func fakeTool(t *testing.T, name, body string) []string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "PATH="+d+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestHomeAppBuildShowsCodesignsOutputWhenItFails(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rubi-home")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outdir := t.TempDir()
	cmd := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), "v1.2.3", "universal", bin, outdir)
	cmd.Env = append(fakeTool(t, "codesign", `echo "no identity found: boom" >&2; exit 1`), "RUBI_APP_SIGN=adhoc")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no identity found: boom") {
		t.Fatalf("err %v, output %q", err, out)
	}
	if es, _ := os.ReadDir(outdir); len(es) != 0 {
		t.Fatalf("files left: %v", es)
	}
}

// A zip that fails halfway leaves neither a partial zip nor a temporary one, and does not touch the good zip that was there.
func TestHomeAppBuildWritesTheZipAtomically(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rubi-home")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	outdir := t.TempDir()
	final := filepath.Join(outdir, "rubi-home-v1.2.3-macos-universal.zip")
	if err := os.WriteFile(final, []byte("the earlier zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(root(t), "scripts", "build-home-app.sh"), "v1.2.3", "universal", bin, outdir)
	cmd.Env = fakeTool(t, "zip", `for a in "$@"; do case "$a" in *.zip) echo partial > "$a" ;; esac; done; exit 1`)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("a failing zip was fine:\n%s", out)
	}
	if b, _ := os.ReadFile(final); string(b) != "the earlier zip" {
		t.Fatalf("the earlier zip was replaced by %q", b)
	}
	if es, _ := os.ReadDir(outdir); len(es) != 1 {
		t.Fatalf("temporary files left: %v", es)
	}
}
