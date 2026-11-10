package agentsandbox

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// VerifyRuntimeFiles verifies the build manifest before installing a companion.
// The outer release archive is authenticated by Rubi's existing signed sums.
func VerifyRuntimeFiles(root string) error {
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() {
		return errors.New("sandbox: unsafe runtime directory")
	}
	marker, err := os.ReadFile(filepath.Join(root, "COMPLETE"))
	if err != nil || strings.TrimSpace(string(marker)) != RuntimeID() {
		return errors.New("sandbox: runtime completion pin missing")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	f, err := r.Open("SHA256SUMS")
	if err != nil {
		return errors.New("sandbox: runtime checksum manifest missing")
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	seen := map[string]bool{}
	for scan.Scan() {
		line := scan.Text()
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 || !fs.ValidPath(name) {
			return errors.New("sandbox: invalid runtime checksum manifest")
		}
		if seen[name] {
			return errors.New("sandbox: duplicate runtime checksum entry")
		}
		seen[name] = true
		want, err := hex.DecodeString(sum)
		if err != nil {
			return errors.New("sandbox: invalid runtime checksum")
		}
		file, err := r.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		st, err := file.Stat()
		if err != nil || !st.Mode().IsRegular() {
			file.Close()
			return errors.New("sandbox: runtime entry is not a regular file")
		}
		h := sha256.New()
		_, err = io.Copy(h, file)
		file.Close()
		if err != nil || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), hex.EncodeToString(want)) {
			return errors.New("sandbox: runtime checksum mismatch")
		}
	}
	if scan.Err() != nil || len(seen) == 0 {
		return errors.New("sandbox: unreadable runtime checksum manifest")
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("sandbox: runtime symlink refused")
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "COMPLETE" && rel != "SHA256SUMS" && !seen[filepath.ToSlash(rel)] {
			return errors.New("sandbox: unmanifested runtime file")
		}
		return nil
	})
}

// runtimeCarrier is where a release keeps srt-runtime next to the launcher in src. The Mac app keeps it in
// Contents/Resources: codesign treats every file in Contents/MacOS as code, and the runtime's scripts and
// Linux-only helpers can't carry a Mach-O signature.
func runtimeCarrier(src string) string {
	if filepath.Base(src) == "MacOS" && filepath.Base(filepath.Dir(src)) == "Contents" {
		return filepath.Join(filepath.Dir(src), "Resources")
	}
	return src
}

// InstallRuntimeCompanions is shared by the signed-archive and Mac-app installers.
// Immutable pin/platform trees are staged before rename. Existing valid trees
// stay in use; incomplete ones refuse instead of modifying running executables.
func InstallRuntimeCompanions(src, dst string) error {
	carrier := runtimeCarrier(src)
	source := filepath.Join(carrier, "srt-runtime", RuntimeID())
	st, err := os.Lstat(filepath.Join(carrier, "srt-runtime"))
	if os.IsNotExist(err) {
		return nil
	} // old release/development app
	if err != nil || !st.IsDir() {
		return errors.New("sandbox: unsafe release runtime directory")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return errors.New("sandbox: release runtime pin missing")
	}
	if err := validateRuntime(filepath.Join(source, runtimePlatform())); err != nil {
		return err
	}
	target := filepath.Join(dst, "srt-runtime", RuntimeID())
	for _, p := range []string{dst, filepath.Dir(target), target} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return err
		}
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return errors.New("sandbox: unsafe runtime install directory")
		}
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("sandbox: unsafe runtime platform entry")
		}
		name := entry.Name()
		if name != "linux-amd64" && name != "linux-arm64" && name != "darwin-amd64" && name != "darwin-arm64" {
			return errors.New("sandbox: unsupported bundled platform")
		}
		from, to := filepath.Join(source, name), filepath.Join(target, name)
		if err := VerifyRuntimeFiles(from); err != nil {
			return err
		}
		if _, err := os.Lstat(to); err == nil {
			if err := VerifyRuntimeFiles(to); err != nil {
				return err
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		temp, err := os.MkdirTemp(target, ".install-")
		if err != nil {
			return err
		}
		err = filepath.WalkDir(from, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.Type()&os.ModeSymlink != 0 {
				return errors.New("sandbox: runtime symlink refused")
			}
			rel, e := filepath.Rel(from, path)
			if e != nil {
				return e
			}
			dest := filepath.Join(temp, rel)
			if d.IsDir() {
				return os.MkdirAll(dest, 0755)
			}
			st, e := d.Info()
			if e != nil || !st.Mode().IsRegular() {
				return errors.New("sandbox: invalid runtime file")
			}
			return copyRuntimeFile(path, dest, st.Mode().Perm())
		})
		if err == nil {
			err = VerifyRuntimeFiles(temp)
		}
		if err == nil {
			err = os.Rename(temp, to)
		}
		os.RemoveAll(temp)
		if err != nil {
			return err
		}
	}
	relay := filepath.Join(src, "rubi-agent-exec")
	st, err = os.Lstat(relay)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return errors.New("sandbox: release launcher missing")
	}
	temp, err := os.CreateTemp(dst, ".rubi-agent-exec-")
	if err != nil {
		return err
	}
	name := temp.Name()
	temp.Close()
	defer os.Remove(name)
	if err := copyRuntimeFile(relay, name, 0755); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dst, "rubi-agent-exec"))
}
func copyRuntimeFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = out.Sync()
	}
	return errors.Join(err, out.Close())
}
