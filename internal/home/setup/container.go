package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Release is a release of Apple's container tool that Rubi Home installs: pinned in code, so a changed
// download is refused whatever the server says.
type Release struct {
	Version string
	URL     string // https://github.com/apple/container/releases/download/<version>/container-<version>-installer-signed.pkg
	Size    int64
	SHA256  string
	// Team is the Developer ID team that signs the package (and the installed binary).
	Team string
}

// Pinned is the release this Rubi Home installs and upgrades to; it never installs an older one.
var Pinned = Release{
	Version: "1.6.1",
	URL:     "https://github.com/apple/container/releases/download/1.6.1/container-1.6.1-installer-signed.pkg",
	Size:    155965213,
	SHA256:  "cd47846819f06478d067060efe21e516b562ac0a9af8eecad6d5fea0b556b200",
	Team:    "UPBK2H6LZM",
}

func (r Release) fileName() string { return "container-" + r.Version + "-installer-signed.pkg" }

// ---- download ----

var errDownload = errors.New("the download doesn't match the pinned release")

// download fetches the release into dir (0700) over HTTPS, at most the pinned size, and checks size and
// SHA-256; a mismatch deletes the file. An existing file that matches is kept. It returns the file's path.
func (s *Setup) download(ctx context.Context, dir string, log func(string)) (string, error) {
	rel := s.opt.Release
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, rel.fileName())
	if fileMatches(final, rel) == nil {
		log("The package is already downloaded and matches.")
		return final, nil
	}
	_ = os.Remove(final)
	u, err := url.Parse(rel.URL)
	if err != nil || u.Scheme != "https" || !slices.Contains(s.opt.DownloadHosts, u.Host) || u.User != nil {
		return "", errors.New("the pinned download address isn't an https address on an allowed host")
	}
	client := http.DefaultClient
	if s.opt.HTTP != nil {
		client = s.opt.HTTP
	}
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil || !slices.Contains(s.opt.RedirectHosts, req.URL.Host) {
			return fmt.Errorf("the download was redirected to %s, which isn't allowed", clip(req.URL.Hostname(), 64))
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.URL, nil)
	if err != nil {
		return "", err
	}
	log("Downloading " + rel.fileName())
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed: %s", safeText(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: the server answered %d", resp.StatusCode)
	}
	if resp.ContentLength > rel.Size {
		return "", fmt.Errorf("%w: it is larger than %d bytes", errDownload, rel.Size)
	}
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer os.Remove(part) // gone after the rename; deleted on any failure
	h := sha256.New()
	var n, shown int64
	buf := make([]byte, 256<<10)
	for {
		m, rerr := resp.Body.Read(buf)
		if m > 0 {
			n += int64(m)
			if n > rel.Size {
				f.Close()
				return "", fmt.Errorf("%w: it is larger than %d bytes", errDownload, rel.Size)
			}
			h.Write(buf[:m])
			if _, err := f.Write(buf[:m]); err != nil {
				f.Close()
				return "", err
			}
			if n-shown >= rel.Size/10 {
				shown = n
				log(fmt.Sprintf("Downloaded %d of %d MB", n>>20, rel.Size>>20))
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return "", fmt.Errorf("download failed: %s", safeText(rerr.Error()))
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if n != rel.Size {
		return "", fmt.Errorf("%w: its size is %d bytes", errDownload, n)
	}
	if hex.EncodeToString(h.Sum(nil)) != rel.SHA256 {
		return "", fmt.Errorf("%w: its SHA-256 is wrong", errDownload)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	log("The download matches the pinned size and SHA-256.")
	return final, nil
}

// fileMatches checks an existing file against the release; a mismatch deletes it.
func fileMatches(path string, rel Release) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != rel.Size {
		_ = os.Remove(path)
		return errDownload
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != rel.SHA256 {
		_ = os.Remove(path)
		return errDownload
	}
	return nil
}

// ---- signature ----

// checkSignature parses `pkgutil --check-signature`: a package signed by Apple's Developer ID program, trusted
// by the notary service, whose leaf certificate (1.) is a "Developer ID Installer" of the team.
func checkSignature(out, team string) error {
	if !strings.Contains(out, "Status: signed by a developer certificate issued by Apple for distribution") {
		return errors.New("the package isn't signed by a developer certificate issued by Apple")
	}
	if !strings.Contains(out, "Notarization: trusted by the Apple notary service") {
		return errors.New("the package isn't notarized")
	}
	leaf := regexp.MustCompile(`(?m)^\s*1\.\s+Developer ID Installer: .*\(` + regexp.QuoteMeta(team) + `\)\s*$`)
	if !leaf.MatchString(out) {
		return fmt.Errorf("the package isn't signed by a Developer ID Installer certificate of team %s", team)
	}
	return nil
}

// checkAssessment parses `spctl --assess --type install -vv`.
func checkAssessment(out, team string) error {
	if !strings.Contains(out, ": accepted") || !strings.Contains(out, "source=Notarized Developer ID") {
		return errors.New("macOS doesn't accept the package as a notarized Developer ID package")
	}
	if !regexp.MustCompile(`(?m)^origin=Developer ID Installer: .*\(` + regexp.QuoteMeta(team) + `\)\s*$`).MatchString(out) {
		return fmt.Errorf("macOS doesn't show the package as from team %s", team)
	}
	return nil
}

func (s *Setup) verifySignature(ctx context.Context, pkg string) error {
	if !s.mac() {
		return errNotMac
	}
	out, err := s.opt.Cmd.Run(ctx, []string{"/usr/sbin/pkgutil", "--check-signature", pkg})
	if err != nil {
		return fmt.Errorf("the signature check failed: %s", safeText(string(out)))
	}
	if err := checkSignature(string(out), s.opt.Release.Team); err != nil {
		return err
	}
	out, err = s.opt.Cmd.Run(ctx, []string{"/usr/sbin/spctl", "--assess", "--type", "install", "-vv", pkg})
	if err != nil {
		return fmt.Errorf("macOS rejected the package: %s", safeText(string(out)))
	}
	return checkAssessment(string(out), s.opt.Release.Team)
}

// ---- administrator install ----

// installScript runs as root (osascript, "with administrator privileges") as `sh -c <script> rubi-install
// <pkg> <sha256> <team> <uid>`, uid being 0. It copies the package into a fresh directory under
// /private/var/tmp (TMPDIR is dropped: it is not root's to trust), checks that the directory is a real
// directory of that uid with mode 700, so a process of the user can't swap the file after it was checked,
// checks the SHA-256 and the signature of the copy, installs it and removes the copy.
const installScript = `set -eu
PATH=/usr/bin:/bin:/usr/sbin:/sbin
export PATH
src=$1 want=$2 team=$3 uid=$4
unset TMPDIR
umask 077
if [ "$(id -u)" != "$uid" ]; then echo "not running as the expected user" >&2; exit 13; fi
if [ ! -f "$src" ] || [ -L "$src" ]; then echo "the package is not a plain file" >&2; exit 10; fi
dir=$(mktemp -d /private/var/tmp/rubi-container.XXXXXX)
trap 'rm -rf "$dir"' EXIT
if [ -L "$dir" ] || [ ! -d "$dir" ] || [ "$(stat -f %u "$dir")" != "$uid" ] || [ "$(stat -f %Lp "$dir")" != 700 ]; then
  echo "the scratch directory is not private" >&2; exit 13
fi
pkg=$dir/container.pkg
cp "$src" "$pkg"
got=$(shasum -a 256 "$pkg" | cut -d' ' -f1)
if [ "$got" != "$want" ]; then echo "the package changed after it was checked" >&2; exit 11; fi
sig=$(pkgutil --check-signature "$pkg") || { echo "the signature check failed" >&2; exit 12; }
printf '%s\n' "$sig" | grep -q 'Status: signed by a developer certificate issued by Apple for distribution' || exit 12
printf '%s\n' "$sig" | grep -q 'Notarization: trusted by the Apple notary service' || exit 12
printf '%s\n' "$sig" | grep -Eq "^ +1\. Developer ID Installer: .*\($team\)\$" || { echo "wrong signing team" >&2; exit 12; }
installer -pkg "$pkg" -target /
`

// shellQuote quotes one word for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// asString quotes text as an AppleScript string literal.
func asString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// installCommand is the osascript argv for the administrator install of pkg. The script is a constant; the
// package path, hash and team are quoted words, so no value can add a command.
func installCommand(pkg, sha, team string) ([]string, error) {
	return installCommandAs(pkg, sha, team, "0")
}

// installCommandAs is installCommand for the user id the script must run as (0 outside tests).
func installCommandAs(pkg, sha, team, uid string) ([]string, error) {
	for _, v := range []string{pkg, sha, team, uid} {
		if v == "" || strings.ContainsAny(v, "\x00\r\n") || strings.ContainsFunc(v, func(r rune) bool { return r < ' ' }) {
			return nil, errors.New("a value for the installer can't be passed safely")
		}
	}
	if !filepath.IsAbs(pkg) {
		return nil, errors.New("the package path isn't absolute")
	}
	sh := "/bin/sh -c " + shellQuote(installScript) + " rubi-install " + shellQuote(pkg) + " " + shellQuote(sha) + " " + shellQuote(team) + " " + shellQuote(uid)
	return []string{"/usr/bin/osascript", "-e", "do shell script " + asString(sh) + " with administrator privileges"}, nil
}

// cancelled says whether osascript's output is the user cancelling the administrator dialog (error -128).
func cancelled(out string) bool {
	return strings.Contains(out, "(-128)") || strings.Contains(out, "User canceled")
}

// installLimit is how long the administrator install may take (the dialog waits for the user).
var installLimit = 30 * time.Minute

// ---- the job ----

func (s *Setup) runContainer(ctx context.Context, j *job) outcome {
	log := j.log(s)
	if !s.mac() {
		return outcome{Failed, "Setting up agents works only on a Mac."}
	}
	rel := s.opt.Release
	want, _ := parseVersion([]byte("version " + rel.Version))
	if v, ok, present := s.installed(ctx); present && !ok {
		// a tool that runs but whose version can't be read is never installed over
		return outcome{Failed, "Unknown container version; not reinstalling."}
	} else if present && v.cmp(want) >= 0 {
		return outcome{Done, "Version " + v.String() + " is installed."} // never reinstall, never downgrade
	}
	pkg, err := s.download(ctx, filepath.Join(s.opt.Dir, "setup"), log)
	if err != nil {
		return outcome{Failed, safeText(err.Error())}
	}
	log("Checking the signature...")
	if err := s.verifySignature(ctx, pkg); err != nil {
		_ = os.Remove(pkg)
		return outcome{Failed, safeText(err.Error())}
	}
	argv, err := installCommand(pkg, rel.SHA256, rel.Team)
	if err != nil {
		return outcome{Failed, err.Error()}
	}
	log("Waiting for the administrator dialog on this Mac...")
	// The privileged script is not stopped by a cancel: killing osascript could leave the installer running
	// in the middle of its work. The job stays busy until the script has ended (the administrator dialog
	// itself can be cancelled by the user); only the time limit stops it.
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installLimit)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { log("Cancel noticed; waiting for the installer to finish.") })
	defer stop()
	out, err := s.opt.Cmd.Run(ictx, argv)
	if err != nil {
		switch {
		case cancelled(string(out)):
			return outcome{NeedsUser, "The administrator dialog was cancelled."}
		case ictx.Err() != nil:
			return outcome{Failed, "The installer didn't finish in " + installLimit.String() + " and was stopped; the installation may be half done."}
		case ctx.Err() != nil:
			return outcome{Failed, "Cancel came too late: the installer had started and ended with an error. " + safeText(string(out))}
		}
		return outcome{Failed, "The installer failed: " + safeText(string(out))}
	}
	_ = os.Remove(pkg)
	if v, ok, present := s.installed(ctx); !present || !ok || v.cmp(want) < 0 {
		return outcome{Failed, "The installer finished, but the container tool isn't at version " + want.String() + "."}
	}
	return outcome{Done, "Installed version " + want.String() + "."}
}
