// Package agenttask owns repository copies outside the provider's writable tree.
// It never publishes, consults credentials, or executes project hooks.
package agenttask

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

var idRE = regexp.MustCompile(`^task_[0-9a-f]{24}$`)

func ValidID(id string) bool { return idRE.MatchString(id) }

// CheckPlatform matches the launcher boundary: Windows has no supported srt
// isolation backend, and repository preparation relies on trusted Unix git.
func CheckPlatform(goos string) error {
	if goos != "linux" && goos != "darwin" {
		return fmt.Errorf("repository tasks are not supported on %s: srt isolation requires Linux or macOS; select a Linux or macOS Rubi Home", goos)
	}
	return nil
}

type Workspace struct {
	ID         string    `json:"id"`
	Project    string    `json:"project"`
	Role       string    `json:"role"`
	SourceHash string    `json:"source_hash"`
	BaseCommit string    `json:"base_commit"`
	Branch     string    `json:"branch"`
	ConfigHash string    `json:"config_hash"`
	Created    time.Time `json:"created"`
	Dir        string    `json:"-"`
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
		return errors.New("task: unsafe state directory")
	}
	return nil
}
func paths(home, id string) (state, dir string, err error) {
	if !ValidID(id) {
		return "", "", errors.New("task: invalid task id")
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil || !filepath.IsAbs(home) {
		return "", "", errors.New("task: Home directory missing")
	}
	return filepath.Join(home, "t3", "tasks", id+".json"), filepath.Join(home, "t3", "workspaces", id), nil
}
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "core.fsmonitor=false", "-c", "core.attributesFile=/dev/null", "-c", "uploadpack.packObjectsHook=", "-C", dir}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file"}
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("task: local git operation failed (no hooks, credentials or network used)")
	}
	return strings.TrimSpace(string(out)), nil
}
func configHash(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", errors.New("task: unsafe git configuration")
	}
	defer root.Close()
	b, err := readMetadata(root, ".git/config", 1<<20)
	if err != nil {
		return "", errors.New("task: unsafe git configuration")
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func readMetadata(root *os.Root, path string, max int64) ([]byte, error) {
	f, err := root.OpenFile(path, os.O_RDONLY|fsx.NonBlock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > max {
		return nil, errors.New("task: unsafe or oversized Git metadata")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("task: unsafe or oversized Git metadata")
	}
	return b, nil
}

// Prepare copies committed HEAD, never dirty/untracked files. --no-local avoids
// shared object inodes/alternates; unlike a linked worktree, git commit needs no
// access to the source repository's common gitdir. The agent gets no remote.
func Prepare(ctx context.Context, home, source, project, role string) (w Workspace, err error) {
	if err = CheckPlatform(runtime.GOOS); err != nil {
		return w, err
	}
	if project == "" || role == "" {
		return w, errors.New("task: missing project or role")
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil || !filepath.IsAbs(source) || roles.DirHash(source) == "" {
		return w, errors.New("task: source project missing")
	}
	if err = validateGitTree(source); err != nil {
		return w, err
	}
	top, err := git(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		return w, err
	}
	top, err = filepath.EvalSymlinks(top)
	if err != nil || top != source {
		return w, errors.New("task: enroll the repository root, not a subdirectory")
	}
	head, err := git(ctx, source, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return w, errors.New("task: repository needs a committed HEAD")
	}
	nonce := make([]byte, 12)
	if _, err = rand.Read(nonce); err != nil {
		return w, err
	}
	w = Workspace{ID: "task_" + hex.EncodeToString(nonce), Project: project, Role: role, SourceHash: roles.DirHash(source), BaseCommit: head, Created: time.Now().UTC()}
	w.Branch = "rubi/" + w.ID
	state, dir, err := paths(home, w.ID)
	if err != nil {
		return w, err
	}
	w.Dir = dir
	for _, p := range []string{filepath.Dir(filepath.Dir(state)), filepath.Dir(state), filepath.Dir(dir)} {
		if err = privateDir(p); err != nil {
			return w, err
		}
	}
	// Reserve an empty destination without ever removing a pre-existing path.
	if err = os.Mkdir(dir, 0700); err != nil {
		return w, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if _, err = git(ctx, source, "clone", "--no-local", "--no-hardlinks", "--no-checkout", "--single-branch", "--", source, dir); err != nil {
		return w, err
	}
	for _, args := range [][]string{{"remote", "remove", "origin"}, {"config", "user.name", "Rubi Task"}, {"config", "user.email", "rubi-task@localhost"}, {"config", "commit.gpgSign", "false"}, {"config", "core.hooksPath", "/dev/null"}, {"checkout", "-b", w.Branch, w.BaseCommit}} {
		if _, err = git(ctx, dir, args...); err != nil {
			return w, err
		}
	}
	w.ConfigHash, err = configHash(dir)
	if err != nil {
		return w, err
	}
	data, err := json.Marshal(w)
	if err != nil {
		return w, err
	}
	f, err := os.CreateTemp(filepath.Dir(state), ".task-binding-")
	if err != nil {
		return w, err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = os.Link(f.Name(), state)
	}

	return w, err
}

// Resolve takes only an opaque ID and the enrolled project's trusted binding.
// No cwd, provider environment, supplied worktree path, or source .git is used.
func Resolve(home, id, project, role, sourceHash string) (w Workspace, err error) {
	if err = CheckPlatform(runtime.GOOS); err != nil {
		return w, err
	}
	if project == "" || role == "" {
		return w, errors.New("task: missing project or role")
	}
	state, dir, err := paths(home, id)
	if err != nil {
		return w, err
	}
	for _, p := range []string{filepath.Dir(state), filepath.Dir(dir), dir, filepath.Join(dir, ".git")} {
		st, e := os.Lstat(p)
		if e != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
			return w, errors.New("task: workspace directory missing or unsafe")
		}
	}
	st, err := os.Lstat(state)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 16<<10 {
		return w, errors.New("task: workspace binding missing or unsafe")
	}
	data, err := os.ReadFile(state)
	if err != nil {
		return w, err
	}
	if json.Unmarshal(data, &w) != nil || w.ID != id || w.Project != project || w.Role != role || w.SourceHash != sourceHash || sourceHash == "" || w.Branch != "rubi/"+id {
		return w, errors.New("task: workspace belongs to another project or role")
	}
	w.Dir = dir
	hash, err := configHash(dir)
	if err != nil || hash != w.ConfigHash {
		return w, errors.New("task: git configuration changed; refusing host-side reuse")
	}
	if err := validateGitTree(dir); err != nil {
		return w, err
	}

	return w, nil
}

// List reads only the trusted state directory; transient writes are hidden names.
func List(home string) ([]Workspace, error) {
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "t3", "tasks")
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
		return nil, errors.New("task: unsafe binding directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Workspace
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if !ValidID(id) || e.Name() != id+".json" {
			return nil, errors.New("task: unexpected workspace binding")
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<10 {
			return nil, errors.New("task: unsafe workspace binding")
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var w Workspace
		if json.Unmarshal(data, &w) != nil || w.ID != id {
			return nil, errors.New("task: invalid workspace binding")
		}
		out = append(out, w)
	}
	return out, nil
}

func validateGitTree(dir string) error {
	st, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil || !st.IsDir() {
		return errors.New("task: independent repository with a real .git directory required")
	}
	for _, p := range []string{"commondir", "objects/info/alternates"} {
		if _, err := os.Lstat(filepath.Join(dir, ".git", p)); !os.IsNotExist(err) {
			return errors.New("task: shared git database refused")
		}
	}
	err = filepath.WalkDir(filepath.Join(dir, ".git"), func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("task: git metadata symlink refused")
		}
		if !d.IsDir() {
			st, e := d.Info()
			if e != nil || !st.Mode().IsRegular() {
				return errors.New("task: non-regular git metadata refused")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}
