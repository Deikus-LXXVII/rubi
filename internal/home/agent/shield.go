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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
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

// shieldSeam, when set by a test, runs between a directory's listing and the opening of its entries: the moment
// a writer VM of the project could swap something for a symlink.
var shieldSeam func(rel string)

// openUnder opens the directory rel (slash-separated, no "..") below root one component at a time, each opened
// relative to the previous directory's handle and checked with Lstat to be a real directory (not a symlink,
// even one that stays inside root). The result is the handle of the last directory.
func openUnder(root *os.Root, rel string) (*os.Root, error) {
	cur, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, c := range strings.Split(filepath.ToSlash(rel), "/") {
		next, err := openChildDir(cur, c)
		cur.Close()
		if err != nil {
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

// openChildDir opens the directory name of parent without following a symlink: Lstat says it is a directory,
// and the opened handle is the very same directory.
func openChildDir(parent *os.Root, name string) (*os.Root, error) {
	lst, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !lst.IsDir() {
		return nil, fmt.Errorf("%s isn't a plain directory", name)
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	st, err := sub.Stat(".")
	if err != nil || !os.SameFile(lst, st) {
		sub.Close()
		return nil, fmt.Errorf("%s changed while it was opened", name)
	}
	return sub, nil
}

// snapshotDir replaces dst with a copy of projectDir/rel made without following symlinks (they are skipped, as
// is anything that isn't a regular file or directory). Every directory is opened relative to its parent's handle,
// so a swap of any level for a symlink can't lead the copy out of the project. A source over the size limits, or
// one swapped while copying, yields an empty copy (copyErr says why): the role still sees the place covered,
// read-only, without the content. err is a failure to make the snapshot at all.
func snapshotDir(projectDir, rel, dst string) (copyErr, err error) {
	tmp := dst + ".new"
	if err := os.RemoveAll(tmp); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return nil, err
	}
	var bytes int64
	var files int
	copyErr = func() error {
		pr, err := os.OpenRoot(projectDir)
		if err != nil {
			return err
		}
		defer pr.Close()
		top, err := openUnder(pr, rel)
		if err != nil {
			return err
		}
		defer top.Close()
		return copyTree(top, tmp, rel, &bytes, &files)
	}()
	if copyErr != nil {
		_ = os.RemoveAll(tmp)
		if err := os.Mkdir(tmp, 0o755); err != nil {
			return copyErr, err
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return copyErr, err
	}
	return copyErr, os.Rename(tmp, dst)
}

func copyTree(root *os.Root, dst, rel string, bytes *int64, files *int) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	ents, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return err
	}
	if shieldSeam != nil {
		shieldSeam(rel)
	}
	for _, e := range ents {
		name, t := e.Name(), filepath.Join(dst, e.Name())
		switch {
		case e.Type().IsDir():
			sub, err := openChildDir(root, name)
			if err != nil {
				return err
			}
			if err := os.Mkdir(t, 0o755); err != nil {
				sub.Close()
				return err
			}
			err = copyTree(sub, t, rel+"/"+name, bytes, files)
			sub.Close()
			if err != nil {
				return err
			}
		case e.Type().IsRegular():
			if *files++; *files > snapshotMaxFiles {
				return errSnapshotTooBig
			}
			lst, err := root.Lstat(name)
			if err != nil || !lst.Mode().IsRegular() {
				continue // gone or swapped for something else: left out
			}
			in, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			st, err := in.Stat()
			if err != nil || !st.Mode().IsRegular() || !os.SameFile(lst, st) {
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

// snapshotKey names one VM's snapshots under RoleShieldDir: the project's directory hashed (its path stays out of
// a file name) and the role's id, so two VMs of one project never share a snapshot, and one VM's start only
// refreshes its own.
func snapshotKey(projectDir, role string) string {
	h := sha256.Sum256([]byte(projectDir))
	return hex.EncodeToString(h[:8]) + "-" + role
}

// removeShield deletes a VM's snapshots (the VM is gone), and sweepShields all of them (only the daemon's start calls it: a command that merely opens the helper's files
// must not remove the snapshots of VMs a running Home has up).
func removeShield(helperDir, projectDir, role string) {
	if helperDir == "" || role == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(helperDir, RoleShieldDir, snapshotKey(projectDir, role)))
}

func sweepShields(helperDir string) {
	if helperDir != "" {
		_ = os.RemoveAll(filepath.Join(helperDir, RoleShieldDir))
	}
}

// guardedFiles are single files a role VM can still write (a file can't be bind-mounted) that another session, the
// host's tools or its editor would load. Home notes them before a role VM starts and says so when one changes.
// The first two are the ones that run code or reach servers; the rest are only fingerprinted the same way.
var guardedFiles = []string{".mcp.json", filepath.Join(".git", "config"), ".gitattributes", ".gitmodules",
	filepath.Join(".vscode", "tasks.json"), ".envrc"}

// guardInterval is how often a running role VM's guarded files are looked at again.
var guardInterval atomic.Int64

func init() { guardInterval.Store(int64(time.Minute)) }

// fingerprint is the size and the SHA-256 of the whole file, read as a stream.
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
	n, err := io.Copy(h, f)
	if err != nil {
		return "unreadable"
	}
	return fmt.Sprintf("%d:%s", n, hex.EncodeToString(h.Sum(nil)))
}

// gitConfigs returns where the repository's configuration files are: .git/config of a plain repository; for a
// worktree (.git is a file "gitdir: PATH") the worktree's own gitdir config and the common dir's, each only when
// the directory is inside the project or is a real git directory (it has a HEAD).
func gitConfigs(projectDir string) map[string]string {
	out := map[string]string{}
	dotGit := filepath.Join(projectDir, ".git")
	st, err := os.Lstat(dotGit)
	if err != nil || st.IsDir() {
		out[filepath.Join(".git", "config")] = filepath.Join(dotGit, "config")
		return out
	}
	if !st.Mode().IsRegular() || st.Size() > 4096 {
		return out
	}
	b, err := os.ReadFile(dotGit)
	if err != nil {
		return out
	}
	gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return out
	}
	gd = strings.TrimSpace(gd)
	if !filepath.IsAbs(gd) {
		gd = filepath.Join(projectDir, gd)
	}
	gd = filepath.Clean(gd)
	if !usableGitDir(projectDir, gd) {
		return out
	}
	out["gitdir:config"] = filepath.Join(gd, "config")
	if cb, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil && len(cb) < 4096 {
		cd := strings.TrimSpace(string(cb))
		if !filepath.IsAbs(cd) {
			cd = filepath.Join(gd, cd)
		}
		if cd = filepath.Clean(cd); usableGitDir(projectDir, cd) {
			out["gitdir:common-config"] = filepath.Join(cd, "config")
		}
	}
	return out
}

func usableGitDir(projectDir, dir string) bool {
	if rel, err := filepath.Rel(projectDir, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	st, err := os.Lstat(filepath.Join(dir, "HEAD"))
	return err == nil && st.Mode().IsRegular()
}

// fileGuard remembers the guarded files of a project at a role VM's start (and what it already told about).
type fileGuard struct {
	mu       sync.Mutex
	paths    map[string]string // name shown -> path
	start    map[string]string // name -> fingerprint at the start
	reported map[string]string // name -> fingerprint last reported
}

func newFileGuard(projectDir string) *fileGuard {
	g := &fileGuard{paths: map[string]string{}, start: map[string]string{}, reported: map[string]string{}}
	for _, n := range guardedFiles {
		if n == filepath.Join(".git", "config") {
			continue // see gitConfigs
		}
		g.paths[n] = filepath.Join(projectDir, n)
	}
	for n, p := range gitConfigs(projectDir) {
		g.paths[n] = p
	}
	for n, p := range g.paths {
		g.start[n] = fingerprint(p)
	}
	return g
}

// changed lists the guarded files that differ from the start (including appearing or vanishing).
func (g *fileGuard) changed(string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for n, p := range g.paths {
		if fingerprint(p) != g.start[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// fresh returns the files that changed since the start and weren't reported in their present state yet.
func (g *fileGuard) fresh() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for n, p := range g.paths {
		fp := fingerprint(p)
		if fp == g.start[n] {
			delete(g.reported, n) // put back: a later change is news again
			continue
		}
		if g.reported[n] != fp {
			g.reported[n] = fp
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// report logs the files that changed since the last report and returns them.
func (g *fileGuard) report(roleName string) []string {
	ch := g.fresh()
	if len(ch) > 0 {
		log.Printf("agent: WARNING role %q changed %v in its project while its VM ran; review before another session or your editor loads them", clip(roleName, 40), ch)
	}
	return ch
}
