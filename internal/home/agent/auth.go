package agent

// The sign-in shared by all projects. Claude's credential is one long-lived token (`claude setup-token`,
// subscription, scope user:inference). It lives in a directory of Rubi Home on the Mac, <helper dir>/agent-auth
// (0700, token in agent-auth/token/), bind-mounted read-only at /rubi-auth in every project VM (outside /work)
// and read-write only in the short-lived sign-in container. (An Apple container volume is a block device and
// can't be attached to two running VMs; a bind mount can.) The runner reads the token at each query start into
// the SDK child's environment. Only the token is shared: each project keeps its own CLAUDE_CONFIG_DIR.
// Home never reads the token: the status only stats the file (size > 0) and reads the non-secret metadata file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// AuthDirName is the sign-in directory's name inside the helper's directory.
	AuthDirName    = "agent-auth"
	authMountPoint = "/rubi-auth"
	authContainer  = "rubi-agent-auth"
	// AuthHelper is the sign-in helper in the image (runner/container/rubi-claude-token).
	AuthHelper = "/usr/local/bin/rubi-claude-token"
	tokenFile  = "oauth-token"
	metaFile   = "oauth-token.meta"
)

// AuthHosts are the only hosts the sign-in container's proxy allows.
var AuthHosts = []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"}

// AuthDirOf is the sign-in directory for a helper directory.
func AuthDirOf(helperDir string) string {
	if helperDir == "" {
		return ""
	}
	return filepath.Join(helperDir, AuthDirName)
}

// authDir is the host directory of the shared sign-in: AuthDir, else <EnvDir>/agent-auth, else none (then
// nothing is mounted).
func (a *AppleContainer) authDir() string {
	if a.AuthDir != "" {
		return a.AuthDir
	}
	return AuthDirOf(a.EnvDir)
}

// authMount is the --mount value for the sign-in directory ("" when there is none).
func (a *AppleContainer) authMount(readOnly bool) string {
	dir := a.authDir()
	if dir == "" {
		return ""
	}
	m := "type=bind,source=" + dir + ",target=" + authMountPoint
	if readOnly {
		m += ",readonly"
	}
	return m
}

// EnsureAuthDir creates the sign-in directory and its token directory (0700), unless they are there. It
// refuses a path that can't be mounted, a symlink, and a directory someone else owns or others can use.
func (a *AppleContainer) EnsureAuthDir() error {
	dir := a.authDir()
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) || badMountPath(dir) {
		return errors.New("the sign-in directory can't be mounted (it must be an absolute path without commas or control characters)")
	}
	for _, d := range []string{dir, filepath.Join(dir, "token")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		st, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s isn't a plain directory; refusing to use it for the sign-in", d)
		}
		if st.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// AuthStatus is what the shared sign-in looks like from outside: whether a token is there, and the dates of
// its metadata file when it has one. The token itself is never read.
type AuthStatus struct {
	SignedIn bool
	Created  time.Time
	Expires  time.Time // zero when unknown
}

// Expired says whether the token's recorded end of life has passed.
func (s AuthStatus) Expired(now time.Time) bool {
	return s.SignedIn && !s.Expires.IsZero() && now.After(s.Expires)
}

// AuthStatusOf reads the state of a sign-in directory: the token file is only stat'ed (a plain file with a size
// above zero is "signed in"), the metadata file (no secret in it) is read.
func AuthStatusOf(dir string) AuthStatus {
	st, err := os.Lstat(filepath.Join(dir, "token", tokenFile))
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return AuthStatus{}
	}
	out := AuthStatus{SignedIn: true}
	mp := filepath.Join(dir, "token", metaFile)
	if ms, err := os.Lstat(mp); err == nil && ms.Mode().IsRegular() && ms.Size() <= 4096 {
		if f, err := os.Open(mp); err == nil {
			defer f.Close()
			b, _ := io.ReadAll(io.LimitReader(f, 4096))
			var meta struct {
				Created string `json:"created"`
				Expires string `json:"expires"`
			}
			if json.Unmarshal(b, &meta) == nil {
				out.Created, _ = time.Parse(time.RFC3339, meta.Created)
				out.Expires, _ = time.Parse(time.RFC3339, meta.Expires)
			}
		}
	}
	return out
}

// AuthStatus reports the shared sign-in. It runs no container and never reads the token.
func (a *AppleContainer) AuthStatus(ctx context.Context) (AuthStatus, error) {
	dir := a.authDir()
	if dir == "" {
		return AuthStatus{}, errors.New("there is no directory for the sign-in")
	}
	return AuthStatusOf(dir), nil
}

// AuthSignOut deletes the token and its metadata for every project (the next query of a running agent has none).
func (a *AppleContainer) AuthSignOut(ctx context.Context) error {
	dir := a.authDir()
	if dir == "" {
		return errors.New("there is no directory for the sign-in")
	}
	var errs []error
	for _, f := range []string{tokenFile, metaFile} {
		if err := os.Remove(filepath.Join(dir, "token", f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SweepAuth removes a sign-in container left by a crash. It is for Rubi Home's start.
func (a *AppleContainer) SweepAuth(ctx context.Context) {
	_, _ = a.do(ctx, []string{a.bin(), "delete", "--force", authContainer})
}

// AuthSession is the running sign-in container and its proxy.
type AuthSession struct {
	a     *AppleContainer
	px    *Proxy
	env   map[string]string
	ended bool
}

// StartAuth starts the sign-in container: no project, the directory read-write, the host-only network, and a
// proxy on the Mac's address on that network that allows only AuthHosts. It fails closed like a VM start.
func (a *AppleContainer) StartAuth(ctx context.Context) (*AuthSession, error) {
	if a.authMount(false) == "" {
		return nil, errors.New("there is no directory for the sign-in")
	}
	if err := a.EnsureImage(ctx); err != nil {
		return nil, err
	}
	gateway, err := a.verifyInternal(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.EnsureAuthDir(); err != nil {
		return nil, err
	}
	del := []string{a.bin(), "delete", "--force", authContainer}
	_, _ = a.do(ctx, del) // a leftover of a crash
	run := []string{a.bin(), "run", "-d", "--progress", "none", "--name", authContainer, "--label", "rubi.role=auth", "--init",
		"--network", a.network(), "--user", runnerUID, "--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp",
		"--mount", a.authMount(false), "--entrypoint", "sleep", a.image(), "infinity"}
	if _, err := a.do(ctx, run); err != nil {
		return nil, err
	}
	token, err := NewToken()
	if err != nil {
		_, _ = a.do(context.WithoutCancel(ctx), del)
		return nil, err
	}
	listen := net.JoinHostPort(gateway, "0")
	if a.authListen != "" {
		listen = a.authListen
	}
	px, err := StartProxy(ProxyConfig{Listen: listen, AuthToken: token, Hosts: AuthHosts, Logf: log.Printf})
	if err != nil {
		_, _ = a.do(context.WithoutCancel(ctx), del)
		return nil, fmt.Errorf("proxy: %w", err)
	}
	_, port, _ := net.SplitHostPort(px.Addr())
	u := "http://rubi:" + token + "@" + net.JoinHostPort(gateway, port)
	return &AuthSession{a: a, px: px, env: map[string]string{"HTTPS_PROXY": u, "https_proxy": u}}, nil
}

// envFileGrace is how long the exec's environment file (it holds the proxy password) lives after the exec
// started, long enough for the container tool to have read it.
var envFileGrace = 2 * time.Second

// Exec runs argv in the sign-in container with the proxy variables and extra. Its stderr is dropped.
func (s *AuthSession) Exec(ctx context.Context, argv []string, extra map[string]string) (Conn, error) {
	env := map[string]string{}
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	c, err := s.a.execNamed(ctx, authContainer, env, argv, false)
	if err != nil {
		return nil, err
	}
	if r, ok := c.(ReadyNotifier); ok {
		time.AfterFunc(envFileGrace, r.Ready) // the environment file is not kept for the whole sign-in
	}
	return c, nil
}

// Close ends the sign-in: the proxy, then the container with everything running in it.
func (s *AuthSession) Close() {
	if s.ended {
		return
	}
	s.ended = true
	s.px.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.a.do(ctx, []string{s.a.bin(), "stop", "--time", "2", authContainer})
	_, _ = s.a.do(ctx, []string{s.a.bin(), "delete", "--force", authContainer})
}
