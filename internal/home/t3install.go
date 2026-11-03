package home

// Installing the pinned T3 into Rubi Home's folder (docs/plans/t3-integration.md section 13): Node, the T3 npm
// package and the T3 executable the package's launcher would otherwise download on its first run. Everything comes
// from a fixed address, is checked against a hash that is pinned in t3/upstream.json (the npm package against the
// sha512 integrity, the other two against SHA-256) before a byte of it is unpacked, and nothing from a download is
// run until it has passed. Downloads go through a seam so tests use local files.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
	rt3 "github.com/Deikus-LXXVII/rubi/t3"
)

// t3Pin is what t3/upstream.json pins.
type t3Pin struct {
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	Runtime   struct {
		SHA256 map[string]string `json:"sha256"`
	} `json:"runtime"`
	Node struct {
		Version string            `json:"version"`
		SHA256  map[string]string `json:"sha256"`
	} `json:"node"`
}

func loadT3Pin() (t3Pin, error) {
	var p t3Pin
	if err := json.Unmarshal(rt3.Upstream, &p); err != nil || p.Version == "" || p.Integrity == "" {
		return p, errors.New("the pinned T3 version is missing from this build")
	}
	return p, nil
}

// pin is the pinned T3 and Node (t3/upstream.json; tests set Pin).
func (t *T3Home) pin() (t3Pin, error) {
	if t.Pin != nil {
		return *t.Pin, nil
	}
	return loadT3Pin()
}

// FetchFunc copies the resource at url to w, reading at most max bytes (more is an error). It is the only way the
// installer reaches the network.
type FetchFunc func(ctx context.Context, url string, w io.Writer, max int64) error

// httpFetch is the real download: HTTPS only (redirects too), no other scheme.
func httpFetch(ctx context.Context, rawurl string, w io.Writer, max int64) error {
	u, err := url.Parse(rawurl)
	if err != nil || u.Scheme != "https" {
		return errors.New("downloads must be https")
	}
	hc := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 8 {
			return errors.New("refusing a redirect")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", u.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", u.Host, resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", u.Host, err)
	}
	if n > max {
		return fmt.Errorf("download %s: larger than the %d bytes expected at most", u.Host, max)
	}
	return nil
}

// t3Platform is the platform key of the pins ("darwin-arm64", "linux-x64"...), or "" on a platform with no T3 build.
func t3Platform() string {
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[runtime.GOARCH]
	switch {
	case runtime.GOOS == "darwin" && arch == "arm64", runtime.GOOS == "linux" && arch != "":
		return runtime.GOOS + "-" + arch
	}
	return ""
}

// ---- what is installed ----

type t3Installed struct {
	T3Version string `json:"t3_version"`
	Integrity string `json:"t3_integrity"`
	Node      string `json:"node_version"`
	Platform  string `json:"platform"`
	RuntimeSH string `json:"runtime_sha256"`
	NodeSH    string `json:"node_sha256"`
	At        string `json:"at"`
}

func (t *T3Home) runtimeDir() string  { return filepath.Join(t.root(), "runtime") }
func (t *T3Home) nodeBin() string     { return filepath.Join(t.runtimeDir(), "node", "bin", "node") }
func (t *T3Home) pkgBin() string      { return filepath.Join(t.runtimeDir(), "pkg", "bin", "t3.mjs") }
func (t *T3Home) runtimeHome() string { return filepath.Join(t.runtimeDir(), "t3home") }
func (t *T3Home) installedFile() string {
	return filepath.Join(t.runtimeDir(), "installed.json")
}

func (t *T3Home) exePath(version string) string {
	return filepath.Join(t.runtimeHome(), "runtime", "versions", version, "t3")
}

// installedNow says whether the folder holds exactly the pinned T3 and Node for this platform, complete.
func (t *T3Home) installedNow() (t3Installed, bool) {
	pin, err := t.pin()
	plat := t.platform()
	if err != nil || plat == "" {
		return t3Installed{}, false
	}
	b, err := os.ReadFile(t.installedFile())
	var in t3Installed
	if err != nil || json.Unmarshal(b, &in) != nil {
		return in, false
	}
	if in.T3Version != pin.Version || in.Integrity != pin.Integrity || in.Node != pin.Node.Version || in.Platform != plat ||
		in.RuntimeSH != pin.Runtime.SHA256[plat] || in.NodeSH != pin.Node.SHA256[plat] {
		return in, false
	}
	marker, err := os.ReadFile(filepath.Join(t.runtimeHome(), "runtime", "versions", pin.Version, ".install-complete"))
	if err != nil || strings.TrimSpace(string(marker)) != pin.Version {
		return in, false
	}
	for _, f := range []string{t.nodeBin(), t.pkgBin(), t.exePath(pin.Version)} {
		if st, err := os.Stat(f); err != nil || !st.Mode().IsRegular() {
			return in, false
		}
	}
	return in, true
}

// ---- downloading and checking ----

// countingWriter lets the status say how far a download is.
type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// fetchChecked downloads url into dir/name, hashing as it goes, and returns the path only when the hash matches. The
// file is removed otherwise. algo is "sha256" (want is hex) or "sha512" (want is the base64 of an npm integrity).
func (t *T3Home) fetchChecked(ctx context.Context, dir, name, rawurl, algo, want string, max int64) (string, error) {
	var h hash.Hash
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return "", errors.New("unknown hash")
	}
	if want == "" {
		return "", fmt.Errorf("no pinned hash for %s", name)
	}
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	fetch := t.Fetch
	if fetch == nil {
		fetch = httpFetch
	}
	err = fetch(ctx, rawurl, countingWriter{io.MultiWriter(f, h), &t.bytes}, max)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return "", err
	}
	var got string
	if algo == "sha256" {
		got = hex.EncodeToString(h.Sum(nil))
	} else {
		got = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	if got != want {
		os.Remove(p)
		return "", fmt.Errorf("%s does not match the pinned hash; nothing was unpacked or run", name)
	}
	return p, nil
}

// ---- unpacking ----

const (
	maxUnpackEntries = 400000
	maxUnpackBytes   = 2 << 30
	maxUnpackFile    = 1 << 30
)

// unpackTarGz unpacks a .tar.gz into dest, dropping strip leading path components. It refuses a path that leaves dest,
// an absolute path, a link that points out of dest and any entry that is not a folder, a file or a link inside dest.
func unpackTarGz(file, dest string, strip int) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return err
	}
	within := func(p string) bool { return p == realDest || strings.HasPrefix(p, realDest+string(filepath.Separator)) }
	tr := tar.NewReader(gz)
	var entries int
	var total int64
	for {
		hd, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if entries++; entries > maxUnpackEntries {
			return errors.New("the archive has too many entries")
		}
		name, ok := stripped(hd.Name, strip)
		if !ok {
			return fmt.Errorf("the archive has a path outside its folder: %q", clipStr(hd.Name, 60))
		}
		if name == "" {
			continue
		}
		target := filepath.Join(realDest, filepath.FromSlash(name))
		if !within(target) {
			return fmt.Errorf("the archive has a path outside its folder: %q", clipStr(hd.Name, 60))
		}
		// What is already there must not lead out through a link made by an earlier entry.
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if real, err := filepath.EvalSymlinks(filepath.Dir(target)); err != nil || !within(real) {
			return fmt.Errorf("the archive writes through a link: %q", clipStr(hd.Name, 60))
		}
		switch hd.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hd.Size > maxUnpackFile {
				return errors.New("the archive has a file that is too large")
			}
			if total += hd.Size; total > maxUnpackBytes {
				return errors.New("the archive unpacks to too much")
			}
			if err := writeUnpacked(target, tr, hd.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(hd.Linkname) || hd.Linkname == "" {
				return fmt.Errorf("the archive has a link that is not relative: %q", clipStr(hd.Name, 60))
			}
			if !within(filepath.Join(filepath.Dir(target), filepath.FromSlash(hd.Linkname))) {
				return fmt.Errorf("the archive has a link that leaves its folder: %q", clipStr(hd.Name, 60))
			}
			os.Remove(target)
			if err := os.Symlink(hd.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src, ok := stripped(hd.Linkname, strip)
			if !ok || src == "" {
				return fmt.Errorf("the archive has a bad hard link: %q", clipStr(hd.Name, 60))
			}
			from := filepath.Join(realDest, filepath.FromSlash(src))
			if !within(from) {
				return fmt.Errorf("the archive has a link that leaves its folder: %q", clipStr(hd.Name, 60))
			}
			if real, err := filepath.EvalSymlinks(from); err != nil || !within(real) {
				return fmt.Errorf("the archive has a link that leaves its folder: %q", clipStr(hd.Name, 60))
			}
			os.Remove(target)
			if err := os.Link(from, target); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			return fmt.Errorf("the archive has an entry of a kind that is not unpacked: %q", clipStr(hd.Name, 60))
		}
	}
}

// stripped drops n leading components of a slash path; ok is false for a path that is absolute or has "..".
func stripped(name string, n int) (string, bool) {
	if path.IsAbs(name) || strings.Contains(name, "\x00") {
		return "", false
	}
	parts := strings.Split(path.Clean(name), "/")
	for _, p := range parts {
		if p == ".." {
			return "", false
		}
	}
	if len(parts) <= n {
		return "", true
	}
	rest := strings.Join(parts[n:], "/")
	if rest == "." {
		rest = ""
	}
	return rest, true
}

func writeUnpacked(target string, r io.Reader, mode os.FileMode) error {
	os.Remove(target)
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- the install ----

// Install downloads, checks and unpacks the pinned Node, T3 package and T3 executable into <helper>/t3/runtime. It
// stages everything beside the final folder and swaps it in only when all three passed; a server must not be running
// from the folder.
func (t *T3Home) Install(ctx context.Context) error {
	pin, err := t.pin()
	if err != nil {
		return err
	}
	plat := t.platform()
	if plat == "" {
		return fmt.Errorf("T3 has no build for this computer (%s/%s); supported: macOS on Apple silicon, Linux on arm64 or x64", runtime.GOOS, runtime.GOARCH)
	}
	nodeSum, rtSum := pin.Node.SHA256[plat], pin.Runtime.SHA256[plat]
	if nodeSum == "" || rtSum == "" || pin.Node.Version == "" {
		return fmt.Errorf("this build pins no Node or T3 download for %s", plat)
	}
	if t.srvAlive() {
		return errors.New("T3 is running; stop it before installing")
	}
	root := t.root()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	tmpID, err := randHex()
	if err != nil {
		return err
	}
	stage := filepath.Join(root, "runtime.new-"+tmpID)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	dl, err := os.MkdirTemp(root, "download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dl)

	nv := pin.Node.Version
	t.setPhase("downloading Node " + nv)
	nodeTgz, err := t.fetchChecked(ctx, dl, "node.tar.gz", fmt.Sprintf("https://nodejs.org/dist/%s/node-%s-%s.tar.gz", nv, nv, plat), "sha256", nodeSum, 400<<20)
	if err != nil {
		return err
	}
	t.setPhase("unpacking Node")
	if err := unpackTarGz(nodeTgz, filepath.Join(stage, "node"), 1); err != nil {
		return fmt.Errorf("Node: %w", err)
	}
	os.Remove(nodeTgz)

	t.setPhase("downloading the T3 package " + pin.Version)
	sri := strings.TrimPrefix(pin.Integrity, "sha512-")
	if sri == pin.Integrity {
		return errors.New("the pinned integrity is not a sha512 value")
	}
	pkgTgz, err := t.fetchChecked(ctx, dl, "t3.tgz", fmt.Sprintf("https://registry.npmjs.org/t3/-/t3-%s.tgz", pin.Version), "sha512", sri, 20<<20)
	if err != nil {
		return err
	}
	if err := unpackTarGz(pkgTgz, filepath.Join(stage, "pkg"), 1); err != nil {
		return fmt.Errorf("T3 package: %w", err)
	}
	os.Remove(pkgTgz)
	if err := checkPackage(filepath.Join(stage, "pkg"), pin.Version); err != nil {
		return err
	}

	t.setPhase("downloading T3 " + pin.Version)
	rtTgz, err := t.fetchChecked(ctx, dl, "t3rt.tar.gz", fmt.Sprintf("https://github.com/pingdotgg/t3code/releases/download/v%s/t3-%s-%s.tar.gz", pin.Version, pin.Version, plat), "sha256", rtSum, 800<<20)
	if err != nil {
		return err
	}
	t.setPhase("unpacking T3")
	vdir := filepath.Join(stage, "t3home", "runtime", "versions", pin.Version)
	if err := unpackTarGz(rtTgz, vdir, 1); err != nil {
		return fmt.Errorf("T3 executable: %w", err)
	}
	os.Remove(rtTgz)
	if st, err := os.Stat(filepath.Join(vdir, "t3")); err != nil || !st.Mode().IsRegular() {
		return errors.New("the T3 archive holds no t3 executable")
	}
	// The launcher runs the executable only when this marker says the install is complete.
	if err := os.WriteFile(filepath.Join(vdir, ".install-complete"), []byte(pin.Version+"\n"), 0o600); err != nil {
		return err
	}
	rec, _ := json.Marshal(t3Installed{T3Version: pin.Version, Integrity: pin.Integrity, Node: nv, Platform: plat,
		RuntimeSH: rtSum, NodeSH: nodeSum, At: time.Now().UTC().Format(time.RFC3339)})
	if err := vault.WriteFileAtomic(filepath.Join(stage, "installed.json"), rec, 0o600); err != nil {
		return err
	}

	t.setPhase("putting it in place")
	dst := t.runtimeDir()
	old := dst + ".old-" + tmpID
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Rename(dst, old); err != nil {
			return err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(stage, dst); err != nil {
		if _, serr := os.Lstat(old); serr == nil {
			_ = os.Rename(old, dst) // put the earlier install back
		}
		return err
	}
	return nil
}

// checkPackage confirms the unpacked npm package is T3 at the pinned version with the launcher in place.
func checkPackage(dir, version string) error {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	var p struct{ Name, Version string }
	if err != nil || json.Unmarshal(b, &p) != nil || p.Name != "t3" || p.Version != version {
		return errors.New("the T3 package is not the pinned one")
	}
	if st, err := os.Stat(filepath.Join(dir, "bin", "t3.mjs")); err != nil || !st.Mode().IsRegular() {
		return errors.New("the T3 package has no launcher")
	}
	return nil
}

func randHex() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
