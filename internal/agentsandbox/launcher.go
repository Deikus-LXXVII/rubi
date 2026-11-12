package agentsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Mode's zero value is sandboxed. Unsandboxed is reserved for slice 13;
// approval and UI do not exist yet, so requests currently fail closed.
type Mode uint8

const (
	Sandboxed Mode = iota
	Unsandboxed
)

type Launch struct {
	Policy      Policy
	SettingsDir string
	// ConfigPath is checked when invoked by the binary adapter.
	ConfigPath string
	SRT, CLI   string
	Args       []string
	// SearchPath is trusted installation configuration, not inherited PATH.
	SearchPath      []string
	Mode            Mode
	Protected       []string
	Provider, Cwd   string
	CredentialsFile string
	// Control is a supervisor-owned srt lifetime pipe, never a provider descriptor.
	Control *os.File
	// OwnProcessTree requires a kernel-owned lifetime for all detached descendants.
	OwnProcessTree bool
}

func environment(parent []string, home, tmp, path string) []string {
	env := []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + path, "SHELL=/bin/bash"}
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "TZ":
			env = append(env, entry)
		}
	}
	return env
}

func executable(path string) (string, error) {
	v, e := canonical(path)
	if e != nil {
		return "", e
	}
	info, e := os.Stat(v)
	if e != nil {
		return "", e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("sandbox dependency is not executable")
	}
	return v, nil
}
func find(name string, dirs []string) (string, error) {
	for _, dir := range dirs {
		p, e := executable(filepath.Join(dir, name))
		if e == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("sandbox dependency %s missing; install it before launching", name)
}

// Run inherits only explicitly safe environment entries and propagates streams
// and the child's exit status. No settings or credentials are logged.
func Run(ctx context.Context, l Launch, in io.Reader, out, stderr io.Writer) (int, error) {
	if l.Mode != Sandboxed {
		return 0, fmt.Errorf("no-sandbox mode requires slice 13 approval support")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return 0, fmt.Errorf("srt unsupported on this platform")
	}
	self, e := os.Executable()
	if e != nil {
		return 0, e
	}
	l, e = ResolveRuntime(l, self)
	if e != nil {
		return 0, e
	}
	srt, e := executable(l.SRT)
	if e != nil {
		return 0, fmt.Errorf("srt missing or not executable: %w", e)
	}
	cli, e := executable(l.CLI)
	if e != nil {
		return 0, fmt.Errorf("provider CLI missing or not executable: %w", e)
	}
	if len(l.SearchPath) == 0 {
		return 0, fmt.Errorf("trusted sandbox search path is required")
	}
	protected := append([]string{l.SettingsDir, srt, cli}, l.Protected...)
	if l.ConfigPath != "" {
		protected = append(protected, l.ConfigPath)
	}
	protected = append(protected, self)
	for _, dir := range l.SearchPath {
		if !filepath.IsAbs(dir) {
			return 0, fmt.Errorf("sandbox PATH entries must be absolute")
		}
		protected = append(protected, dir)
	}
	deps := []string{"node", "rg"}
	if runtime.GOOS == "linux" {
		deps = append(deps, "bwrap", "socat")
	} else {
		deps = append(deps, "sandbox-exec")
	}
	for _, dep := range deps {
		p, e := find(dep, l.SearchPath)
		if e != nil {
			if dep == "bwrap" {
				return 0, errors.New("sandbox: bwrap missing: system bubblewrap is required on Linux; install it before starting an agent (no unconfined fallback)")
			}
			return 0, e
		}
		protected = append(protected, p)
	}
	// The caller provisions this directory outside all writable trees. Never
	// mkdir it here: a policy error must not write into an untrusted location.
	info, e := os.Lstat(l.SettingsDir)
	if e != nil {
		return 0, e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return 0, fmt.Errorf("settings directory must be private (0700) and not a symlink")
	}
	// srt creates proxy Unix sockets below TMPDIR. Role/config paths can exceed
	// sockaddr_un's path limit; use a short private outer runtime directory while
	// preserving the provider's configured writable tmp inside the sandbox.
	proxyTmp, e := os.MkdirTemp("/tmp", "rs-")
	if e != nil {
		return 0, e
	}
	defer os.RemoveAll(proxyTmp)
	protected = append(protected, proxyTmp)
	envCLI, e := executable("/usr/bin/env")
	if e != nil {
		return 0, e
	}
	protected = append(protected, envCLI)
	settings, e := Build(l.Policy, protected)
	if e != nil {
		return 0, e
	}
	f, e := os.CreateTemp(l.SettingsDir, "srt-*.json")
	if e != nil {
		return 0, e
	}
	defer os.Remove(f.Name())
	e = json.NewEncoder(f).Encode(settings)
	closeErr := f.Close()
	if e != nil {
		return 0, e
	}
	if closeErr != nil {
		return 0, closeErr
	}
	tmp, _ := canonical(l.Policy.Tmp)
	args := []string{"--settings", f.Name()}
	if l.Control != nil {
		args = append(args, "--control-fd", "3")
	}
	args = append(args, "--", envCLI, "TMPDIR="+tmp, cli)
	args = append(args, l.Args...)
	cmd := exec.CommandContext(ctx, srt, args...)
	if runtime.GOOS == "linux" {
		cmd.Cancel = nil
	}
	if l.Control != nil {
		cmd.ExtraFiles = []*os.File{l.Control}
	}
	cmd.Dir = settings.Filesystem.AllowWrite[0]
	if l.Cwd != "" {
		cwd, err := canonical(l.Cwd)
		if err != nil {
			return 0, err
		}
		if !within(cmd.Dir, cwd) {
			return 0, fmt.Errorf("session cwd is outside the bound project")
		}
		cmd.Dir = cwd
	}
	home, _ := canonical(l.Policy.Home)

	cmd.Env = environment(os.Environ(), home, proxyTmp, strings.Join(l.SearchPath, string(os.PathListSeparator)))
	switch l.Provider {
	case "claudeAgent":
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+l.Policy.Login, "CLAUDE_SECURESTORAGE_CONFIG_DIR="+l.Policy.Login)
	case "codex":
		cmd.Env = append(cmd.Env, "CODEX_HOME="+l.Policy.Login)
	}
	cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+l.Policy.Login, "XDG_DATA_HOME="+l.Policy.Login, "XDG_CACHE_HOME="+l.Policy.Login)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = stderr
	e = runConfined(ctx, cmd, l.OwnProcessTree)
	if e == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) {
		code := exit.ExitCode()
		if code < 0 {
			return 1, nil
		}
		return code, nil
	}
	return 0, fmt.Errorf("start sandbox: %w", e)
}
