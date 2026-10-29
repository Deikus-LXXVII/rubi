package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// A Bot hands Rubi its routine's webhook key as a file it wrote itself (key_file), never as a tool
// argument: the agent's host redacts known secrets inside argument substitutions (Grok Bot turned
// "$file:<path>" into "[REDACTED]"), and an argument ends up in transcripts. The daemon reads the file:
// it serves the MCP tools, and `rubi mcp` only relays stdio to its Unix socket, so both run on the same
// machine. The socket is 0600 inside the 0700 RUBI_HOME, so only the daemon's own user can call a tool,
// and the file must belong to that user too.

const maxKeyFile = 4 << 10

// keyFileDirs are the only directories a key file may be in: the agent's scratch dirs, not Rubi's home
// or the user's. The process temp dir ($TMPDIR) is allowed as well (see keyFileRoots).
var keyFileDirs = []string{"/workspace", "/tmp"}

var keyFileTempDir = os.TempDir

func keyFileRoots() []string {
	roots := append([]string{}, keyFileDirs...)
	if t := filepath.Clean(keyFileTempDir()); !slices.Contains(roots, t) {
		roots = append(roots, t)
	}
	return roots
}

// ReadKeyFile reads a webhook key from path and deletes the file, whether or not the key is usable.
// The path must be absolute and clean, inside keyFileDirs (after resolving links in its directory),
// and name a regular file (not a link) owned by this user, mode 0600 or stricter, at most 4 KB.
// Surrounding whitespace is trimmed. Errors never contain the file's contents.
func ReadKeyFile(path string) (string, error) {
	if err := checkKeyFilePath(path); err != nil {
		return "", err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("key_file %s: %w", path, errors.Unwrap(err))
	}
	if !ownedByMe(fi) {
		return "", fmt.Errorf("key_file %s isn't owned by the user Rubi runs as; write it yourself with printenv", path)
	}
	// Ours and in an allowed directory: it goes away whatever happens next (a link is removed, not its target).
	defer os.Remove(path)
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("key_file %s is a symlink; write the key into the file itself", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("key_file %s: %w", path, errors.Unwrap(err))
	}
	defer f.Close()
	fi, err = f.Stat() // the file actually opened, not what the path named a moment ago
	if err != nil {
		return "", err
	}
	switch {
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("key_file %s isn't a regular file", path)
	case !ownedByMe(fi):
		return "", fmt.Errorf("key_file %s isn't owned by the user Rubi runs as", path)
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("key_file %s is readable by others (mode %04o); write it after umask 077 (mode 0600)", path, fi.Mode().Perm())
	case fi.Size() > maxKeyFile:
		return "", fmt.Errorf("key_file %s is larger than 4 KB; it should hold only the key", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return "", fmt.Errorf("key_file %s: %w", path, errors.Unwrap(err))
	}
	if len(b) > maxKeyFile {
		return "", fmt.Errorf("key_file %s is larger than 4 KB; it should hold only the key", path)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("key_file %s is empty; check the env var is set ([ -n \"$RUBI_WEBHOOK_KEY_<NAME>\" ]) before printenv", path)
	}
	return key, nil
}

func checkKeyFilePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("key_file must be an absolute, clean path such as /workspace/.rubi-hook-key, not %q", path)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err == nil {
		for _, root := range keyFileRoots() {
			if r, err := filepath.EvalSymlinks(root); err == nil && (dir == r || strings.HasPrefix(dir, r+string(filepath.Separator))) {
				return nil
			}
		}
	}
	return fmt.Errorf("key_file must be inside %s (links resolved), not %s", strings.Join(keyFileRoots(), " or "), path)
}

func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
