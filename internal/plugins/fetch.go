package plugins

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/integrity"
	"github.com/Deikus-LXXVII/rubi/internal/update"
)

// Candidate is a downloaded and verified plugin release, extracted to a staging directory and waiting
// for the user's approval.
type Candidate struct {
	Manifest   Manifest
	Reviewed   bool
	Source     string // where updates come from: a GitHub repository URL or a release base URL
	Base       string // the release this candidate was downloaded from
	SumsSHA256 string
	Dir        string // staging directory
	Tree       string // tree hash of Dir
}

// Expect constrains what a download must match.
type Expect struct {
	PublisherKey string // required key ("" accepts the manifest's key: sideloading, first install)
	SumsSHA256   string // required digest of SHA256SUMS (reviewed versions)
	ID           string // required plugin id
}

// ReleaseBase turns a source and version into the URL prefix of the release files ("" as version means
// the latest release). GitHub repositories map to their releases; any other static host serves
// <source>/latest/ and <source>/<version>/.
func ReleaseBase(source, version string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(source))
	if err != nil || u.Host == "" {
		return "", errors.New("the source must be a URL, like https://github.com/owner/repo")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
		return "", errors.New("the source must use https")
	}
	if u.Host == "github.com" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 {
			repo := "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
			if version == "" {
				return repo + "/releases/latest/download/", nil
			}
			return repo + "/releases/download/" + version + "/", nil
		}
	}
	base := strings.TrimRight(u.String(), "/") + "/"
	if version == "" {
		return base + "latest/", nil
	}
	if !update.ValidVersion(version) {
		return "", errors.New("invalid version")
	}
	return base + version + "/", nil
}

// NormalizeSource returns the canonical form of a GitHub repository URL (or the URL itself).
func NormalizeSource(source string) string {
	u, err := url.Parse(strings.TrimSpace(source))
	if err == nil && u.Host == "github.com" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 {
			return "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
		}
	}
	return strings.TrimSpace(source)
}

// ReadManifest downloads and checks a release's manifest without verifying the package (used to look
// for updates; installation verifies everything).
func ReadManifest(ctx context.Context, base string) (*Manifest, error) {
	b, err := integrity.Fetch(ctx, base+"rubi-plugin.json")
	if err != nil {
		return nil, fmt.Errorf("plugin manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New("plugin manifest is not valid JSON")
	}
	return &m, Check(&m)
}

// Download fetches a plugin release from base, verifies it and extracts it below stagingRoot.
func Download(ctx context.Context, base, stagingRoot string, want Expect) (*Candidate, error) {
	sums, err1 := integrity.Fetch(ctx, base+"SHA256SUMS")
	sig, err2 := integrity.Fetch(ctx, base+"SHA256SUMS.sig")
	rawManifest, err3 := integrity.Fetch(ctx, base+"rubi-plugin.json")
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("download plugin release: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		return nil, errors.New("plugin manifest is not valid JSON")
	}
	if err := Check(&m); err != nil {
		return nil, err
	}
	if want.ID != "" && m.ID != want.ID {
		return nil, fmt.Errorf("the release is plugin %q, expected %q", m.ID, want.ID)
	}
	if want.PublisherKey != "" && strings.TrimSpace(m.Publisher.Key) != strings.TrimSpace(want.PublisherKey) {
		return nil, errors.New("the release is signed by a different publisher key than expected; refusing to install")
	}
	if err := verifySig(m.Publisher.Key, sums, sig); err != nil {
		return nil, errors.New("the release checksums are not signed by the publisher key; refusing to install")
	}
	digest := sha256Hex(sums)
	if want.SumsSHA256 != "" && !strings.EqualFold(digest, want.SumsSHA256) {
		return nil, errors.New("the release differs from the reviewed one in the catalog; refusing to install")
	}
	if h, ok := integrity.Lookup(sums, "rubi-plugin.json"); !ok || h != sha256Hex(rawManifest) {
		return nil, errors.New("the manifest doesn't match the signed checksums")
	}

	name, sum := "", ""
	for _, n := range []string{fmt.Sprintf("%s-%s-%s.tar.gz", m.ID, runtime.GOOS, runtime.GOARCH), m.ID + "-any.tar.gz"} {
		if h, ok := integrity.Lookup(sums, n); ok {
			name, sum = n, h
			break
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%s has no build for %s/%s", m.Name, runtime.GOOS, runtime.GOARCH)
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(stagingRoot, ".staging-"+m.ID+"-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Candidate, error) {
		os.RemoveAll(dir)
		return nil, err
	}
	archive := dir + ".tar.gz"
	defer os.Remove(archive)
	if err := update.DownloadFile(ctx, base+name, archive); err != nil {
		return fail(err)
	}
	if got, err := integrity.HashFile(archive); err != nil || got != sum {
		return fail(errors.New("checksum mismatch for the downloaded plugin; refusing to install"))
	}
	if err := extract(archive, dir); err != nil {
		return fail(fmt.Errorf("unpack plugin: %w", err))
	}
	if err := os.WriteFile(filepath.Join(dir, "rubi-plugin.json"), rawManifest, 0o644); err != nil {
		return fail(err)
	}
	if st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(m.Entry))); err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return fail(fmt.Errorf("the package has no executable %q", m.Entry))
	}
	tree, err := TreeHash(dir)
	if err != nil {
		return fail(err)
	}
	return &Candidate{Manifest: m, Base: base, SumsSHA256: digest, Dir: dir, Tree: tree}, nil
}

const (
	maxFiles     = 2000
	maxTotalSize = 500 << 20
)

// extract unpacks regular files and directories only: no links, devices, absolute paths or "..".
func extract(archive, dst string) error {
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
	var files int
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || name == "rubi-plugin.json" {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			total += h.Size
			if files > maxFiles || total > maxTotalSize {
				return errors.New("package too large")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, h.Size)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry %q (only files and directories)", h.Name)
		}
	}
}
