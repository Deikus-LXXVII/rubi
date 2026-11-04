package repocheck

// The release-version rule (docs/design/rubi-home.md) in the two shell installers, really run: panel/install-home.sh and
// panel/install.sh. The scripts run under sh with a fake curl first on PATH that serves files from a temp dir, a throwaway
// release key swapped in for RELEASE_KEY (as the Windows CI job does), HOME and the install dirs inside the temp dir, and
// stand-ins for launchctl, systemctl and pkill. Nothing outside the temp dir is touched. Skipped when openssl has no Ed25519.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

const fakeCurl = `#!/bin/sh
# Serves https://host/path from $FAKE_NET/host/path; -o FILE writes there, otherwise stdout. A missing file is curl's exit 22.
out=""
url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    --proto) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
path=${url#https://}
path=${path%%\?*}
[ -f "$FAKE_NET/$path" ] || { echo "fake curl: no $url" >&2; exit 22; }
if [ -n "$out" ]; then cp "$FAKE_NET/$path" "$out"; else cat "$FAKE_NET/$path"; fi
`

type shTool struct {
	name    string // "home" or "core"
	script  string
	binary  string // the program inside the archive
	assetPf string // release asset prefix
	envDir  string // the variable that points the install into the temp dir
	rel     string // install dir, under HOME
}

var shTools = []shTool{
	{"home", "panel/install-home.sh", "rubi-home", "rubi-home-", "RUBI_HOME_INSTALL", ".rubi-home"},
	{"core", "panel/install.sh", "rubi", "rubi-", "RUBI_HOME", ".rubi"},
}

type shRig struct {
	t      *testing.T
	tool   shTool
	dir    string
	net    string
	key    string
	script string
	shell  string // sh, and dash where it exists: the scripts must run under both
}

func newShRig(t *testing.T, tool shTool) *shRig {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("shell installers run on macOS and Linux")
	}
	plat := runtime.GOOS + "-" + runtime.GOARCH
	if tool.name == "core" && plat != "linux-amd64" && plat != "linux-arm64" && plat != "darwin-arm64" {
		t.Skip("install.sh does not support " + plat)
	}
	for _, c := range []string{"sh", "openssl", "tar"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
	r := &shRig{t: t, tool: tool, dir: t.TempDir(), shell: "sh"}
	r.net = filepath.Join(r.dir, "net")
	r.key = filepath.Join(r.dir, "key.pem")
	if out, err := exec.Command("openssl", "genpkey", "-algorithm", "ed25519", "-out", r.key).CombinedOutput(); err != nil {
		t.Skipf("openssl has no Ed25519: %v %s", err, out)
	}
	pub, err := exec.Command("openssl", "pkey", "-in", r.key, "-pubout").Output()
	if err != nil {
		t.Skip("openssl pkey -pubout failed")
	}
	pubLine := strings.Split(strings.TrimSpace(string(pub)), "\n")[1]

	src := readRepo(t, tool.script)
	real := regexp.MustCompile(`(?m)^MCowBQYDK2Vw[A-Za-z0-9+/=]+$`)
	if len(real.FindAllString(src, -1)) != 1 {
		t.Fatal("RELEASE_KEY not found exactly once in " + tool.script)
	}
	src = real.ReplaceAllString(src, pubLine)
	if tool.name == "home" { // the Claude Code hook place is outside the temp dir: point it at nothing
		if !strings.Contains(src, "hook=/usr/local/bin/rubi-home\n") {
			t.Fatal("hook path not found")
		}
		src = strings.Replace(src, "hook=/usr/local/bin/rubi-home\n", "hook="+filepath.Join(r.dir, "no-hook")+"\n", 1)
	}
	r.script = filepath.Join(r.dir, "script.sh")
	r.write(r.script, src, 0o644)

	shims := filepath.Join(r.dir, "shims")
	r.write(filepath.Join(shims, "curl"), fakeCurl, 0o755)
	for _, n := range []string{"launchctl", "systemctl", "loginctl"} {
		r.write(filepath.Join(shims, n), "#!/bin/sh\nexit 0\n", 0o755)
	}
	r.write(filepath.Join(shims, "pkill"), "#!/bin/sh\nexit 1\n", 0o755)
	if tool.name == "core" { // cloudflared is already there, at the pinned version
		m := regexp.MustCompile(`CLOUDFLARED_VERSION="([^"]+)"`).FindStringSubmatch(src)
		r.write(filepath.Join(r.dir, "home", ".rubi", "bin", "cloudflared"), "#!/bin/sh\necho cloudflared version "+m[1]+"\n", 0o755)
	}
	return r
}

func (r *shRig) write(p, content string, mode os.FileMode) {
	r.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *shRig) sign(data []byte) []byte {
	r.t.Helper()
	in := filepath.Join(r.dir, "sign.in")
	out := filepath.Join(r.dir, "sign.out")
	r.write(in, string(data), 0o644)
	if o, err := exec.Command("openssl", "pkeyutl", "-sign", "-inkey", r.key, "-rawin", "-in", in, "-out", out).CombinedOutput(); err != nil {
		r.t.Skipf("openssl can't sign Ed25519 with -rawin: %v %s", err, o)
	}
	b, _ := os.ReadFile(out)
	return b
}

func (r *shRig) put(host, path string, b []byte) {
	r.t.Helper()
	r.write(filepath.Join(r.net, host, path), string(b), 0o644)
}

func (r *shRig) relDir(v string) string { return "Deikus-LXXVII/rubi/releases/download/" + v + "/" }

func fakeArchive(t *testing.T, name, version string) []byte {
	t.Helper()
	body := "#!/bin/sh\ncase \"$1\" in\nversion) echo " + version + " ;;\ntransport) echo 'transport: gateway' ;;\n*) echo \"fake " + name + " $*\" ;;\nesac\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// release publishes a genuine, signed release. extra lines are added to SHA256SUMS before it is signed.
func (r *shRig) release(v string, extra ...string) {
	r.t.Helper()
	plat := runtime.GOOS + "-" + runtime.GOARCH
	var sums strings.Builder
	line := func(b []byte, name string) {
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	for _, t := range shTools {
		a := fakeArchive(r.t, t.binary, v)
		name := t.assetPf + v + "-" + plat + ".tar.gz"
		r.put("github.com", r.relDir(v)+name, a)
		line(a, name)
	}
	line([]byte("app"), "rubi-home-"+v+"-macos-universal.zip")
	line([]byte("bin"), "bin/rubi-linux-amd64") // no version in the name: ignored
	for _, e := range extra {
		line([]byte(e), e)
	}
	r.signedSums(v, sums.String())
}

func (r *shRig) signedSums(v, sums string) {
	r.put("github.com", r.relDir(v)+"SHA256SUMS", []byte(sums))
	r.put("github.com", r.relDir(v)+"SHA256SUMS.sig", r.sign([]byte(sums)))
}

func feedJSON(v string) []byte {
	return []byte("{\n  \"version\": \"" + v + "\",\n  \"notes_url\": \"https://example/" + v + "\"\n}\n")
}

// feed publishes latest.json; signed also publishes latest.json.sig.
func (r *shRig) feed(v string, signed bool) {
	r.t.Helper()
	b := feedJSON(v)
	r.put("rubi-panel.com", "releases/latest.json", b)
	sig := filepath.Join(r.net, "rubi-panel.com", "releases", "latest.json.sig")
	_ = os.Remove(sig)
	if signed {
		r.put("rubi-panel.com", "releases/latest.json.sig", r.sign(b))
	}
}

func (r *shRig) binPath() string {
	return filepath.Join(r.dir, "home", r.tool.rel, "bin", r.tool.binary)
}

// installed is the version the installed program reports, "" when there is none.
func (r *shRig) installed() string {
	out, err := exec.Command(r.binPath(), "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (r *shRig) run(args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command(r.shell, append([]string{r.script}, args...)...)
	cmd.Dir = r.dir
	tmpdir := filepath.Join(r.dir, "tmp")
	_ = os.MkdirAll(tmpdir, 0o755)
	cmd.Env = []string{
		"PATH=" + filepath.Join(r.dir, "shims") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + filepath.Join(r.dir, "home"), "TMPDIR=" + tmpdir, "FAKE_NET=" + r.net, "LANG=C",
		r.tool.envDir + "=" + filepath.Join(r.dir, "home", r.tool.rel),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *shRig) mustRun(args ...string) string {
	r.t.Helper()
	out, err := r.run(args...)
	if err != nil {
		r.t.Fatalf("%v\n%s", err, out)
	}
	return out
}

func (r *shRig) mustRefuse(want string, args ...string) {
	r.t.Helper()
	before := r.installed()
	out, err := r.run(args...)
	if err == nil {
		r.t.Fatalf("expected a refusal containing %q, the script succeeded:\n%s", want, out)
	}
	if !strings.Contains(out, want) {
		r.t.Fatalf("refusal without %q:\n%s", want, out)
	}
	if r.installed() != before {
		r.t.Fatalf("the installed copy changed from %q to %q", before, r.installed())
	}
}

func eachShTool(t *testing.T, f func(t *testing.T, r *shRig)) {
	for _, tool := range shTools {
		for _, shell := range []string{"sh", "dash"} {
			if _, err := exec.LookPath(shell); err != nil {
				continue
			}
			t.Run(tool.name+"-"+shell, func(t *testing.T) {
				t.Parallel()
				r := newShRig(t, tool)
				r.shell = shell
				f(t, r)
			})
		}
	}
}

func TestShellInstallersTakeTheVersionFromTheSignedFeed(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.1.0")
		r.release("v0.2.0")
		r.feed("v0.2.0", true)
		r.mustRun()
		if got := r.installed(); got != "v0.2.0" {
			t.Fatalf("installed %q", got)
		}
	})
}

func TestShellInstallersRefuseATamperedFeed(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.1.0")
		r.release("v0.2.0")
		r.feed("v0.2.0", true)
		r.put("rubi-panel.com", "releases/latest.json", feedJSON("v0.1.0")) // the version changed, the old signature stays
		r.mustRefuse("the release feed is not signed with the Rubi release key")
		if r.installed() != "" {
			t.Fatal("something was installed")
		}
	})
}

func TestShellInstallersRefuseAFeedWithoutASignature(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.2.0")
		r.feed("v0.2.0", false)
		r.mustRefuse("the release feed is not signed with the Rubi release key")
		if r.installed() != "" {
			t.Fatal("something was installed")
		}
	})
}

func TestShellInstallersNeverInstallAnOlderRelease(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.1.0")
		r.release("v0.2.0")
		r.feed("v0.2.0", true)
		r.mustRun()
		// An explicit older version, genuinely signed.
		r.mustRefuse("older than the installed v0.2.0", "v0.1.0")
		// The feed replayed to the older release, with that feed's own valid signature.
		r.feed("v0.1.0", true)
		r.mustRefuse("older than the installed v0.2.0")
		if got := r.installed(); got != "v0.2.0" {
			t.Fatalf("installed %q", got)
		}
	})
}

func TestShellInstallersAllowTheSameVersionAgain(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.2.0")
		r.feed("v0.2.0", true)
		r.mustRun("v0.2.0")
		r.mustRun()
		r.mustRun("v0.2.0")
		if got := r.installed(); got != "v0.2.0" {
			t.Fatalf("installed %q", got)
		}
	})
}

// The order is the one update.Newer has: numbers (not text), a release over its pre-release, pre-release names as text.
func TestShellInstallersOrderVersionsLikeUpdateNewer(t *testing.T) {
	cases := []struct {
		have, want string
		ok         bool
	}{
		{"v0.9.0", "v0.10.0", true},
		{"v0.10.0", "v0.9.0", false},
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.4", "v1.2.3", false},
		{"v1.0.0-rc1", "v1.0.0", true},
		{"v1.0.0", "v1.0.0-rc1", false},
		{"v1.0.0-rc1", "v1.0.0-rc2", true},
		{"v1.0.0-rc2", "v1.0.0-rc1", false},
		{"v1.0.0-rc10", "v1.0.0-rc9", true}, // compared as text, like update.Newer: rc9 sorts after rc10
		{"v1.0.0-rc9", "v1.0.0-rc10", false},
		{"v1.0.0-rc1", "v1.0.0-rc1", true},
	}
	eachShTool(t, func(t *testing.T, r *shRig) {
		for _, c := range cases {
			r.release(c.have)
			r.release(c.want)
			// Start over with the installed copy at c.have.
			_ = os.RemoveAll(filepath.Join(r.dir, "home", r.tool.rel, "bin", r.tool.binary))
			r.mustRun(c.have)
			if c.ok {
				r.mustRun(c.want)
				if got := r.installed(); got != c.want {
					t.Errorf("%s -> %s: installed %q", c.have, c.want, got)
				}
			} else {
				r.mustRefuse("older than the installed "+c.have, c.want)
			}
		}
	})
}

func TestShellInstallersRefuseSignedChecksumsOfAnotherVersion(t *testing.T) {
	eachShTool(t, func(t *testing.T, r *shRig) {
		r.release("v0.1.0")
		r.release("v0.2.0")
		// v0.2.0's directory serves v0.1.0's genuine signed list: no archive for v0.2.0 in it.
		old, _ := os.ReadFile(filepath.Join(r.net, "github.com", r.relDir("v0.1.0"), "SHA256SUMS"))
		r.signedSums("v0.2.0", string(old))
		r.mustRefuse("not installing", "v0.2.0")
		// The archive of v0.2.0 is listed, with an asset of another version beside it: the list does not bind the version.
		plat := runtime.GOOS + "-" + runtime.GOARCH
		a := fakeArchive(t, r.tool.binary, "v0.2.0")
		h := sha256.Sum256(a)
		mixed := hex.EncodeToString(h[:]) + "  " + r.tool.assetPf + "v0.2.0-" + plat + ".tar.gz\n" +
			strings.Repeat("a", 64) + "  rubi-home-v0.1.0-macos-universal.zip\n"
		r.signedSums("v0.2.0", mixed)
		r.mustRefuse("the signed checksums are for v0.1.0, not v0.2.0", "v0.2.0")
		if r.installed() != "" {
			t.Fatal("something was installed")
		}
	})
}

func TestShellInstallersGoOnWhenTheInstalledVersionCannotBeRead(t *testing.T) {
	cases := map[string]struct{ body, say string }{
		"dev build": {"#!/bin/sh\necho dev\n", "development build"},
		"broken":    {"#!/bin/sh\nexit 3\n", "can't read the installed version"},
		"hangs":     {"#!/bin/sh\nexec sleep 30\n", "can't read the installed version"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			eachShTool(t, func(t *testing.T, r *shRig) {
				r.release("v0.1.0")
				r.write(r.binPath(), c.body, 0o755)
				out := r.mustRun("v0.1.0")
				if !strings.Contains(out, c.say) {
					t.Errorf("no word about the installed copy (%q):\n%s", c.say, out)
				}
				if got := r.installed(); got != "v0.1.0" {
					t.Errorf("installed %q", got)
				}
			})
		})
	}
}

// The scripts' steps, in the order the rule needs: the feed's signature is checked before its version is read; the older-release
// check and the signed-list check come before anything is installed.
func TestShellInstallersFollowTheReleaseVersionRuleInOrder(t *testing.T) {
	for _, tool := range shTools {
		src := readRepo(t, tool.script)
		steps := []string{
			`-o "$tmp/latest.json.sig" "$feed.sig"`,
			`-in "$tmp/latest.json" -sigfile "$tmp/latest.json.sig"`,
			`version=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$tmp/latest.json"`,
			`release_newer "$have" "$version"`,
			`-in "$tmp/SHA256SUMS" -sigfile "$tmp/SHA256SUMS.sig"`,
			`[ -z "$other" ] || fail "the signed checksums are for $other, not $version; not installing"`,
			`tar -xzf "$tmp/$archive"`,
		}
		last := -1
		for _, st := range steps {
			i := strings.Index(src, st)
			if i < 0 {
				t.Errorf("%s lacks %q", tool.script, st)
				continue
			}
			if i < last {
				t.Errorf("%s: %q comes too early", tool.script, st)
			}
			last = i
		}
		if strings.Count(src, `"version": *"`) != 1 {
			t.Errorf("%s reads the version from text more than once", tool.script)
		}
		for _, bad := range []string{"RUBI_ALLOW", "--force", "ALLOW_DOWNGRADE"} {
			if strings.Contains(src, bad) {
				t.Errorf("%s has %q: the older-release refusal has no override", tool.script, bad)
			}
		}
	}
}

// pages.yml signs the release feed with the release key in the step that has the key, checks the signature, and removes the key.
func TestPagesSignsTheReleaseFeed(t *testing.T) {
	y := readRepo(t, ".github/workflows/pages.yml")
	step := y[strings.Index(y, "- name: Plugin catalog"):]
	step = step[:strings.Index(step[3:], "\n      - name:")+3]
	// a trap removes the key on any exit too (a failed signature ends the step before the rm)
	trap := `trap 'rm -f "$RUNNER_TEMP/key.pem"' EXIT`
	if !strings.Contains(step, trap) || strings.Index(step, trap) > strings.Index(step, `> "$RUNNER_TEMP/key.pem"`) {
		t.Error("the key file is not removed by a trap set before it is written")
	}
	step = strings.Replace(step, trap, "", 1)
	order(t, step,
		`printf '%s\n' "$RELEASE_SIGNING_KEY" > "$RUNNER_TEMP/key.pem"`,
		`-rawin -in panel/releases/latest.json \`,
		`-out panel/releases/latest.json.sig`,
		`rm -f "$RUNNER_TEMP/key.pem"`,
		`-pubin -inkey release-key.pub.pem -rawin -in panel/releases/latest.json \`,
		`-sigfile panel/releases/latest.json.sig`)
	if strings.Count(step, `rm -f "$RUNNER_TEMP/key.pem"`) != 1 {
		t.Error("the key file must be removed once, after every signature")
	}
	if !strings.Contains(y, `json.load(open("panel/releases/latest.json"))["version"]`) {
		t.Error("the Nostr hint no longer reads the feed")
	}
	// the feed is made before it is signed
	if strings.Index(y, "> panel/releases/latest.json") > strings.Index(y, "-out panel/releases/latest.json.sig") {
		t.Error("the feed is signed before it is made")
	}
}
