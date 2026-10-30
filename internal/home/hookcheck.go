package home

// Inspecting the hook binary Claude Code runs (/usr/local/bin/rubi-home by default) from another process:
// `rubi-home status` and the helper's start-up check can't see the daemon's memory, so they run
// `<hook> version` (bounded, killed on a timeout) and look at the file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DefaultHookBinary is where Claude Code's managed settings run the hook from: root-owned on purpose, so
// no process of the user's can swap it for one that allows everything.
const DefaultHookBinary = "/usr/local/bin/rubi-home"

// HookBinaryTimeout bounds `<hook> version`.
const HookBinaryTimeout = 3 * time.Second

// HookBinaryPath is the hook binary to inspect: RUBI_HOOK_BINARY, else DefaultHookBinary.
func HookBinaryPath() string {
	if p := os.Getenv("RUBI_HOOK_BINARY"); p != "" {
		return p
	}
	return DefaultHookBinary
}

// HookBinary is what InspectHookBinary found.
type HookBinary struct {
	Path    string
	Missing bool
	Version string
	SHA256  string
	// Secure: owned by root, not a symlink, and not writable by group or others.
	Secure  bool
	Owner   int
	Mode    os.FileMode
	Symlink bool
	// WhyInsecure names the directory above the binary that anyone but root could change.
	WhyInsecure string
	VersionE    string // why the version couldn't be read ("" when it was)
}

// limitWriter keeps the first n bytes and drops the rest.
type limitWriter struct {
	b bytes.Buffer
	n int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		w.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// runVersion runs `<path> version` and returns its first line. The process (with its children) is killed
// when the timeout passes, so a hung binary never holds the caller.
func runVersion(path string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Env = []string{privilegedPath}
	out := &limitWriter{n: 256}
	cmd.Stdout = out
	cmd.Stdin = nil
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("it didn't answer `version` within %s", timeout)
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", fmt.Errorf("`version` failed: %v", err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out.b.String()), "\n")
	line = cleanField(line)
	if line == "" {
		return "", errors.New("`version` printed nothing")
	}
	return line, nil
}

// maxHashedFile caps what is read to hash a binary.
const maxHashedFile = 256 << 20

// fileSHA256 is the hex SHA-256 of a regular file of at most maxHashedFile bytes (a FIFO or a device is
// refused before it is opened, so it can't hang the caller).
func fileSHA256(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if fi.Size() > maxHashedFile {
		return "", errors.New("file too large")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxHashedFile+1))
	if err != nil {
		return "", err
	}
	if n > maxHashedFile {
		return "", errors.New("file too large")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// insecureAncestors says why a directory above path is not safe for a root-owned binary ("" when all are):
// every ancestor must be root-owned and not writable by group or others. A symlink among them must itself
// be root's (/var on macOS) and is followed.
func insecureAncestors(path string) string {
	for d := filepath.Dir(path); ; d = filepath.Dir(d) {
		li, err := os.Lstat(d)
		if err != nil {
			return fmt.Sprintf("can't look at %s: %v", d, err)
		}
		if li.Mode()&os.ModeSymlink != 0 {
			if st, ok := li.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
				return d + " is a symlink that root doesn't own"
			}
		}
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			return fmt.Sprintf("%s isn't a directory", d)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return d + " is not owned by root"
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return fmt.Sprintf("%s is writable by group or others (mode %04o)", d, fi.Mode().Perm())
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// InspectHookBinary looks at the hook binary at path.
func InspectHookBinary(path string, timeout time.Duration) HookBinary {
	b := HookBinary{Path: path, Owner: -1}
	li, err := os.Lstat(path)
	if err != nil {
		b.Missing = errors.Is(err, os.ErrNotExist)
		b.VersionE = err.Error()
		return b
	}
	b.Symlink = li.Mode()&os.ModeSymlink != 0
	fi, err := os.Stat(path)
	if err != nil {
		b.VersionE = err.Error()
		return b
	}
	b.Mode = fi.Mode()
	if !fi.Mode().IsRegular() {
		b.VersionE = "not a regular file"
		return b
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		b.Owner = int(st.Uid)
	}
	b.Secure = b.Owner == 0 && !b.Symlink && fi.Mode().Perm()&0o022 == 0
	if b.Secure {
		if why := insecureAncestors(path); why != "" {
			b.Secure, b.WhyInsecure = false, why
		}
	}
	if b.SHA256, err = fileSHA256(path); err != nil {
		b.VersionE = err.Error()
		return b
	}
	if b.Version, err = runVersion(path, timeout); err != nil {
		b.VersionE = err.Error()
	}
	return b
}

// WritePlain prints the hook binary's state for `rubi-home status`; self is this binary's version.
func (b HookBinary) WritePlain(w io.Writer, self string) {
	if b.Missing {
		fmt.Fprintf(w, "Claude Code hook binary: none at %s (see the claude-code plugin's README to set it up)\n", b.Path)
		return
	}
	fmt.Fprintf(w, "Claude Code hook binary: %s\n", b.Path)
	if b.VersionE != "" {
		fmt.Fprintf(w, "  problem: %s\n", b.VersionE)
	}
	if b.Version != "" {
		fmt.Fprintf(w, "  version: %s\n", b.Version)
	}
	if b.SHA256 != "" {
		fmt.Fprintf(w, "  sha256: %s\n", b.SHA256)
	}
	if b.Owner >= 0 || b.Mode != 0 {
		fmt.Fprintf(w, "  owner: uid %d, mode %04o, root-owned and not group/world-writable: %s\n",
			b.Owner, b.Mode.Perm(), map[bool]string{true: "yes", false: "NO"}[b.Secure])
		if b.WhyInsecure != "" {
			fmt.Fprintf(w, "  directories: %s\n", b.WhyInsecure)
		}
		if b.Symlink {
			fmt.Fprintln(w, "  it is a symlink; a hook binary must be a plain file")
		}
	}
	if versionOlder(b.Version, self) {
		fmt.Fprintf(w, "  stale: older than this binary (%s); run `%s`\n", self, InstallHookCommand)
	}
}

// WarnIfHookOlder logs once when the hook binary at path is older than self. It is for the helper's
// start-up, off the hook paths; a missing or unreadable binary is not a warning.
func WarnIfHookOlder(path, self string, logf func(format string, args ...any)) {
	b := InspectHookBinary(path, HookBinaryTimeout)
	if b.Missing || b.Version == "" {
		return
	}
	if versionOlder(b.Version, self) {
		logf("the Claude Code hook binary %s is %s, older than Rubi Home %s: run `%s`", path, b.Version, self, InstallHookCommand)
	}
}
