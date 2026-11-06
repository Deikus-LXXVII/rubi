// Package update finds, downloads, verifies and installs new Rubi releases.
//
// Discovery reads https://rubi-panel.com/releases/latest.json, a static file the site publishes for every
// release (no API rate limits, which matter on agent machines that share an egress IP). The file is signed
// (latest.json.sig) and Latest takes the version only from verified bytes. Installation then trusts nothing
// else from it: it downloads the release's SHA256SUMS and SHA256SUMS.sig, verifies the signature
// with the embedded release key, checks the archive's checksum, smoke-tests the new binary, and only then
// swaps it in, keeping the previous binary for rollback.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/integrity"
)

// DefaultFeed is the static release feed published with the site.
const DefaultFeed = "https://rubi-panel.com/releases/latest.json"

type Release struct {
	Version     string `json:"version"`
	PublishedAt string `json:"published_at,omitempty"`
	NotesURL    string `json:"notes_url,omitempty"`
}

// Feed returns the feed URL; test builds may override it with RUBI_UPDATE_FEED.
func Feed(current string) string {
	if current == "dev" || strings.HasSuffix(current, "-test") {
		if f := os.Getenv("RUBI_UPDATE_FEED"); f != "" {
			return f
		}
	}
	return DefaultFeed
}

// errFeedUnsigned is the one answer to a feed that is missing its signature or has a bad one.
var errFeedUnsigned = errors.New("the release feed is not signed with the Rubi release key")

// Latest reads the release feed. The feed is signed (<feed>.sig, Ed25519 over the exact bytes of the feed, by
// the release key): the version is taken only from verified bytes, so whoever can change the file or the
// response cannot name another release. See "The release-version rule" in docs/design/rubi-home.md.
func Latest(ctx context.Context, current string) (Release, error) {
	feed := Feed(current)
	b, err := integrity.Fetch(ctx, integrity.FreshURL(ctx, feed))
	if err != nil {
		return Release{}, fmt.Errorf("release feed: %w", err)
	}
	sig, err := integrity.Fetch(ctx, integrity.FreshURL(ctx, feed+".sig"))
	if err != nil || integrity.Verify(integrity.TrustedKey(current), b, sig) != nil {
		return Release{}, errFeedUnsigned
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil || !validVersion(r.Version) {
		return Release{}, errors.New("release feed: malformed")
	}
	return r, nil
}

func validVersion(v string) bool {
	_, ok := parse(v)
	return ok && !strings.ContainsAny(v, "/\\ ")
}

type semver struct {
	n   [3]int
	pre string
}

func parse(v string) (semver, bool) {
	v = strings.TrimPrefix(v, "v")
	var s semver
	core, pre, _ := strings.Cut(v, "-")
	s.pre = pre
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return s, false
		}
		s.n[i] = n
	}
	return s, true
}

// Newer reports whether candidate is a newer version than current. Unparseable versions are never newer;
// a development build ("dev") is never offered updates.
func Newer(candidate, current string) bool {
	c, ok1 := parse(candidate)
	cur, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if c.n[i] != cur.n[i] {
			return c.n[i] > cur.n[i]
		}
	}
	switch {
	case c.pre == cur.pre:
		return false
	case c.pre == "":
		return true // v1.0.0 > v1.0.0-rc1
	case cur.pre == "":
		return false
	}
	return c.pre > cur.pre
}

// Compatible reports whether a build of version current satisfies a minimum version (empty means any).
// Development and test builds satisfy every minimum.
func Compatible(current, minimum string) bool {
	if minimum == "" || current == "dev" || strings.HasSuffix(current, "-test") {
		return true
	}
	return !Newer(minimum, current)
}

// ValidVersion reports whether v looks like a release version (v1.2.3 or v1.2.3-rc1).
func ValidVersion(v string) bool { return validVersion(v) }

// Verified is a downloaded, checked release ready to install.
type Verified struct {
	Version string
	Binary  string // path to the extracted, smoke-tested binary
}

// Checksums downloads and verifies the signed checksum list of a release. It's cheap, so it can run
// before asking the user, to show that the release is genuinely signed.
func Checksums(ctx context.Context, current, version string) ([]byte, error) {
	if !validVersion(version) {
		return nil, errors.New("invalid version")
	}
	base := integrity.ReleaseBase(current) + "/" + version + "/"
	sums, err1 := integrity.Fetch(ctx, base+"SHA256SUMS")
	sig, err2 := integrity.Fetch(ctx, base+"SHA256SUMS.sig")
	if err := errors.Join(err1, err2); err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}
	if err := integrity.Verify(integrity.TrustedKey(current), sums, sig); err != nil {
		return nil, errors.New("the release is not signed with the Rubi release key; refusing to update")
	}
	if err := bindVersion(sums, version, fmt.Sprintf("rubi-%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)); err != nil {
		return nil, err
	}
	return sums, nil
}

// versionedAsset matches the release assets whose names carry a version: rubi-<v>-... and rubi-home-<v>-...
// The platform word after the version keeps "v1.0.0-darwin" from reading as a pre-release.
// Names such as bin/rubi-linux-amd64 carry none and are not matched.
var versionedAsset = regexp.MustCompile(`^rubi-(?:home-)?(v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.]+)?)-(?:linux|darwin|macos|windows)-`)

// bindVersion makes the signed list bind the version: it must name the archive for exactly this version,
// and every versioned asset in it must carry this version. A genuine list of another release is refused.
func bindVersion(sums []byte, version, archive string) error {
	found := false
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		name := strings.TrimPrefix(f[1], "*")
		if m := versionedAsset.FindStringSubmatch(name); m != nil && m[1] != version {
			return fmt.Errorf("the signed checksums are for %s, not %s; refusing to update", m[1], version)
		}
		if name == archive {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("the signed checksums do not name %s; refusing to update", archive)
	}
	return nil
}

// Download fetches the release archive for this platform, verifies it against the signed checksums,
// extracts the binary into dir, and checks that it runs and reports the expected version.
func Download(ctx context.Context, current, version, dir string) (*Verified, error) {
	// Never back to an older release: an old, genuinely signed version may have known bugs. Callers refuse
	// this too; the guard keeps a future caller from downgrading. Dev and test builds may go anywhere.
	if current != "dev" && !strings.HasSuffix(current, "-test") && !Newer(version, current) {
		return nil, fmt.Errorf("%s is not newer than the installed %s; Rubi only updates forward", version, current)
	}
	sums, err := Checksums(ctx, current, version)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("rubi-%s-%s-%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
	want, ok := integrity.Lookup(sums, name)
	if !ok {
		return nil, fmt.Errorf("no build for %s/%s in %s", runtime.GOOS, runtime.GOARCH, version)
	}
	archive := filepath.Join(dir, ".rubi-update.tar.gz")
	defer os.Remove(archive)
	if err := DownloadFile(ctx, integrity.ReleaseBase(current)+"/"+version+"/"+name, archive); err != nil {
		return nil, err
	}
	got, err := integrity.HashFile(archive)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, errors.New("checksum mismatch for the downloaded release; refusing to update")
	}
	bin := filepath.Join(dir, "rubi.new")
	if err := extractBinary(archive, bin); err != nil {
		return nil, err
	}
	if err := smokeTest(ctx, bin, version); err != nil {
		os.Remove(bin)
		return nil, err
	}
	return &Verified{Version: version, Binary: bin}, nil
}

// Install swaps the verified binary in for exe and keeps the old one as exe+".prev".
func Install(v *Verified, exe string) error {
	prev := exe + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(exe, prev); err != nil {
		// Hard links can fail across filesystems; fall back to copying.
		if err := copyFile(exe, prev); err != nil {
			return fmt.Errorf("keep previous binary: %w", err)
		}
	}
	return os.Rename(v.Binary, exe)
}

// Rollback restores exe+".prev".
func Rollback(exe string) error {
	prev := exe + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return errors.New("no previous version to roll back to")
	}
	return os.Rename(prev, exe)
}

// DownloadFile saves url to dst (up to 200 MiB).
func DownloadFile(ctx context.Context, url, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := integrity.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", filepath.Base(url), resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 200<<20)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func extractBinary(archive, dst string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return errors.New("release archive has no rubi binary")
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "rubi" && !strings.Contains(h.Name, "..") {
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 200<<20)); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
	}
}

func smokeTest(ctx context.Context, bin, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")} // it only prints its version
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("the new binary doesn't run on this machine: %w", err)
	}
	if strings.TrimSpace(string(out)) != version {
		return fmt.Errorf("the new binary reports version %q, expected %s", strings.TrimSpace(string(out)), version)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
