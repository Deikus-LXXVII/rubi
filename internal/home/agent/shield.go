package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"syscall"
)

// RoleShieldDir (under the helper directory) holds the read-only snapshots a role VM sees over the project's
// .claude and .git/hooks. The VM mounts a Home-owned copy, never the project's own directory: a writer VM of the
// same project can't swap that directory for a symlink between Home's check and the container tool's mount.
const RoleShieldDir = "agent-role-shield"

// shieldTargets are the project directories every role VM sees read-only. They are created in the project when
// missing, so the place can't be left open for a role to fill with something another session loads later. A .git
// that isn't a plain directory (a worktree's gitdir file) gets no hooks overlay: its hooks are elsewhere.
var shieldTargets = []string{".claude", filepath.Join(".git", "hooks")}

const (
	snapshotMaxBytes = 8 << 20
	snapshotMaxFiles = 2000
)

// shieldTargetsOf returns the names to protect in projectDir, creating the missing mount points. An existing
// one that isn't a plain directory (a symlink, a file) is an error: the start refuses rather than mount over it.
func shieldTargetsOf(projectDir string) ([]string, error) {
	var out []string
	for _, name := range shieldTargets {
		if name == filepath.Join(".git", "hooks") {
			st, err := os.Lstat(filepath.Join(projectDir, ".git"))
			if err != nil || !st.IsDir() {
				continue // not a repository with its own .git directory
			}
		}
		p := filepath.Join(projectDir, name)
		st, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return nil, err
			}
			if st, err = os.Lstat(p); err != nil || !st.IsDir() {
				return nil, fmt.Errorf("%s isn't a plain directory; refusing to start", name)
			}
		case err != nil:
			return nil, err
		case !st.IsDir():
			return nil, fmt.Errorf("%s isn't a plain directory; refusing to start", name)
		}
		out = append(out, name)
	}
	return out, nil
}

var errSnapshotTooBig = errors.New("snapshot too big")

// snapshotDir replaces dst with a copy of src made without following symlinks (they are skipped, as is
// anything that isn't a regular file or directory). A source over the size limits, or one swapped for a symlink
// while copying, yields an empty copy: the role still sees the place covered, read-only, without the content.
func snapshotDir(src, dst string) error {
	tmp := dst + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return err
	}
	var bytes int64
	var files int
	if err := copyTree(src, tmp, &bytes, &files); err != nil {
		_ = os.RemoveAll(tmp)
		if err := os.Mkdir(tmp, 0o755); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string, bytes *int64, files *int) error {
	d, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	ents, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, t := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		switch {
		case e.Type().IsDir():
			if err := os.Mkdir(t, 0o755); err != nil {
				return err
			}
			if err := copyTree(s, t, bytes, files); err != nil {
				return err
			}
		case e.Type().IsRegular():
			if *files++; *files > snapshotMaxFiles {
				return errSnapshotTooBig
			}
			in, err := os.OpenFile(s, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			st, err := in.Stat()
			if err != nil || !st.Mode().IsRegular() {
				in.Close()
				continue
			}
			mode := os.FileMode(0o644)
			if st.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if *bytes += st.Size(); *bytes > snapshotMaxBytes {
				in.Close()
				return errSnapshotTooBig
			}
			out, err := os.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				in.Close()
				return err
			}
			_, err = io.Copy(out, io.LimitReader(in, snapshotMaxBytes))
			in.Close()
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// snapshotKey names a project's snapshots under RoleShieldDir without putting its path in a file name.
func snapshotKey(projectDir string) string {
	h := sha256.Sum256([]byte(projectDir))
	return hex.EncodeToString(h[:8])
}

// guardedFiles are single files a role VM can still write (a file can't be bind-mounted) that another session or
// the host's tools would load. Home notes them before a role VM starts and says so in its log when one changed.
var guardedFiles = []string{".mcp.json", filepath.Join(".git", "config")}

func fingerprint(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent"
		}
		return "unreadable"
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "not-regular"
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 4<<20)); err != nil {
		return "unreadable"
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fileGuard remembers the guarded files of a project at a role VM's start.
type fileGuard map[string]string

func newFileGuard(projectDir string) fileGuard {
	g := fileGuard{}
	for _, n := range guardedFiles {
		g[n] = fingerprint(filepath.Join(projectDir, n))
	}
	return g
}

// changed lists the guarded files that differ from the start (including appearing or vanishing).
func (g fileGuard) changed(projectDir string) []string {
	var out []string
	for n, was := range g {
		if fingerprint(filepath.Join(projectDir, n)) != was {
			out = append(out, n)
		}
	}
	return out
}

func (g fileGuard) report(projectDir, roleName string) {
	if ch := g.changed(projectDir); len(ch) > 0 {
		log.Printf("agent: WARNING role %q changed %v in its project while its VM ran; review before another session or your editor loads them", clip(roleName, 40), ch)
	}
}
