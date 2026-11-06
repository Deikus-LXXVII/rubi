package agent

// The sign-in of the T3 roles. Claude's credential is claude's own: the user signs in with the unmodified CLI
// (`claude auth login --claudeai`), which writes `.credentials.json` into the sign-in folder (<helper dir>/agent-signin,
// shared by every role, mounted read-write at its own path). Rubi never stores, reads or proxies that credential: Home
// only stats the file to say whether one is there, and the sign-in container that runs the CLI is the only thing Home
// starts for it. (The earlier design, a long-lived token kept by Home and an auth gateway that added it to requests, is
// gone.)

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// AuthHosts are the only hosts the sign-in container's proxy allows.
var AuthHosts = []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"}

// AuthSession is the running sign-in container and its proxy.
type AuthSession struct {
	a     *AppleContainer
	name  string
	px    *Proxy
	pipe  *pipeSup
	env   map[string]string
	endMu sync.Mutex
	ended bool
}

// startAuth is the sign-in container: no project, one directory mounted read-write, no network, and a proxy that
// allows only AuthHosts, reached through the pipe.
func (a *AppleContainer) startAuth(ctx context.Context, name, label, mount string) (*AuthSession, error) {
	if err := a.EnsureImage(ctx); err != nil {
		return nil, err
	}
	del := []string{a.bin(), "delete", "--force", name}
	_, _ = a.do(ctx, del) // a leftover of a crash
	run := []string{a.bin(), "run", "-d", "--progress", "none", "--name", name, "--label", label, "--init",
		"--network", "none", "--user", runnerUID, "--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp",
		"--mount", mount, "--entrypoint", "sleep", a.image(), "infinity"}
	if _, err := a.do(ctx, run); err != nil {
		return nil, err
	}
	token, err := NewToken()
	if err != nil {
		_, _ = a.do(context.WithoutCancel(ctx), del)
		return nil, err
	}
	// No network: the sign-in's only way out is the pipe, whose proxy allows only AuthHosts.
	ln := netpipe.NewListener("auth-proxy", 16)
	u := "http://rubi:" + token + "@" + PipeProxyAddr
	px, err := StartProxy(ProxyConfig{Listener: ln, AuthToken: token, Hosts: AuthHosts, Logf: log.Printf})
	if err != nil {
		_, _ = a.do(context.WithoutCancel(ctx), del)
		return nil, fmt.Errorf("proxy: %w", err)
	}
	sess := &AuthSession{a: a, px: px, name: name, env: map[string]string{"HTTPS_PROXY": u, "https_proxy": u, "NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1"}}
	pipe, err := a.startPipe(ctx, name, &PipeSpec{Proxy: ln}, func() { sess.Close() })
	if err != nil {
		px.Close()
		_, _ = a.do(context.WithoutCancel(ctx), del)
		return nil, fmt.Errorf("the network pipe: %w", err)
	}
	sess.endMu.Lock()
	sess.pipe = pipe
	ended := sess.ended
	sess.endMu.Unlock()
	if ended { // the pipe gave up while it was being set up
		pipe.stop()
	}
	return sess, nil
}

// ---- the sign-in of the T3 roles ----

const (
	// LoginContainerPrefix starts the name of the container a T3 roles' sign-in runs in; each sign-in adds a random suffix, so
	// one that ends deletes only its own.
	LoginContainerPrefix = "rubi-t3-login-"
	// LoginHelper is the helper in the image that runs `claude auth login` (runner/container/rubi-claude-login).
	LoginHelper = "/usr/local/bin/rubi-claude-login"
	// SignInDirName is the folder every T3 role's claude shares for its sign-in (CLAUDE_SECURESTORAGE_CONFIG_DIR).
	SignInDirName = "agent-signin"
	// CredentialFile is the file claude keeps its sign-in in, inside that folder.
	CredentialFile = ".credentials.json"
)

// SignInDirOf is the shared sign-in folder of a helper directory.
func SignInDirOf(helperDir string) string { return filepath.Join(helperDir, SignInDirName) }

// EnsureSignInDir makes the shared sign-in folder (0700) and returns its real path, which is where it is mounted: the
// same path in the sign-in container and in the role VMs. It refuses a folder that can't be mounted or that is itself a link (a link higher up is followed, so the mount is
// the real folder).
func EnsureSignInDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) || badMountPath(dir) {
		return "", errors.New("the sign-in folder can't be mounted (it must be an absolute path without commas or control characters)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s isn't a plain folder; refusing to use it for the sign-in", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || badMountPath(real) {
		return "", errors.New("the sign-in folder can't be mounted")
	}
	return real, nil
}

// SignedInAt says whether the shared folder holds a credential: the file is only stat'ed, never opened.
func SignedInAt(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, CredentialFile))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// LoginEnv is what the sign-in helper's claude needs besides the proxy: the shared folder as its sign-in storage (the
// same path as on the Mac) and a throwaway config folder in the container's memory.
func LoginEnv(signInDir string) map[string]string {
	return map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": signInDir, "CLAUDE_CONFIG_DIR": "/tmp/claude-login", "HOME": "/tmp"}
}

// StartLogin starts the container the T3 roles' sign-in runs in: like StartAuth (no project, no network but the
// allow-list proxy for the sign-in hosts) with the shared sign-in folder mounted read-write at its own path.
func (a *AppleContainer) StartLogin(ctx context.Context, signInDir string) (*AuthSession, error) {
	real, err := EnsureSignInDir(signInDir)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return a.startAuth(ctx, LoginContainerPrefix+hex.EncodeToString(b), "rubi.role=login", "type=bind,source="+real+",target="+real)
}

// SweepLogin removes the sign-in containers a crash left (every container with the login prefix). It is for Rubi Home's
// start.
func (a *AppleContainer) SweepLogin(ctx context.Context) {
	names, err := a.ListContainers(ctx)
	if err != nil {
		return
	}
	for _, n := range names {
		if strings.HasPrefix(n, LoginContainerPrefix) {
			_, _ = a.do(ctx, []string{a.bin(), "delete", "--force", n})
		}
	}
}

// legacyAuthContainer is the name the earlier design's sign-in container had; one a crash of that version left is removed.
const legacyAuthContainer = "rubi-agent-auth"

// Sweep removes what a crash can have left of the sign-in containers (the T3 roles', and the earlier design's).
func (a *AppleContainer) Sweep(ctx context.Context) {
	_, _ = a.do(ctx, []string{a.bin(), "delete", "--force", legacyAuthContainer})
	a.SweepLogin(ctx)
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
	c, err := s.a.execNamed(ctx, s.name, env, argv, false)
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
	s.endMu.Lock()
	if s.ended {
		s.endMu.Unlock()
		return
	}
	s.ended = true
	pipe := s.pipe
	s.endMu.Unlock()
	pipe.stop()
	s.px.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = s.a.do(ctx, []string{s.a.bin(), "stop", "--time", "2", s.name})
	_, _ = s.a.do(ctx, []string{s.a.bin(), "delete", "--force", s.name})
}
