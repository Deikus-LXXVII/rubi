package agent

// The sign-in shared by all projects. Claude's credential is one long-lived token (`claude setup-token`,
// subscription, scope user:inference); it lives in a container volume Home owns, mounted read-only at
// /rubi-auth in every project VM (outside /work), where the runner reads it at each query start into the SDK
// child's environment. Only the token is shared: each project keeps its own CLAUDE_CONFIG_DIR (settings,
// skills, history, MCP). The sign-in itself runs in a short-lived auth container with the volume read-write,
// no project mounted, and a proxy that allows only the sign-in hosts. Home never reads the token: the status
// probe only tests that the file isn't empty and prints a metadata file with no secret in it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

const (
	// DefaultAuthVolume is the volume of the shared sign-in (config agent_auth_volume).
	DefaultAuthVolume = "rubi-agent-auth"
	authMountPoint    = "/rubi-auth"
	authContainer     = "rubi-agent-auth"
	// AuthHelper is the sign-in helper in the image (runner/container/rubi-claude-token).
	AuthHelper = "/usr/local/bin/rubi-claude-token"
)

// AuthHosts are the only hosts the sign-in container's proxy allows.
var AuthHosts = []string{"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com", "api.anthropic.com"}

func (a *AppleContainer) volume() string {
	if a.AuthVolume != "" {
		return a.AuthVolume
	}
	return DefaultAuthVolume
}

func (a *AppleContainer) authMount(readOnly bool) string {
	m := "type=volume,source=" + a.volume() + ",target=" + authMountPoint
	if readOnly {
		m += ",readonly"
	}
	return m
}

// authSh runs a shell script in a throwaway container: the runner image, as the runner's user, no
// capabilities, a read-only root, the host-only network, the auth volume (read-only unless rw).
func (a *AppleContainer) authSh(rw bool, script string) []string {
	return []string{a.bin(), "run", "--rm", "--progress", "none", "--network", a.network(), "--user", runnerUID, "--cap-drop", "ALL", "--read-only",
		"--tmpfs", "/tmp", "--mount", a.authMount(!rw), "--entrypoint", "sh", a.image(), "-c", script}
}

// authInitArgs prepares a new volume: its root is owned by root, so a one-shot container as root (with only
// the two capabilities chown needs) makes the token directory the runner's user owns.
func (a *AppleContainer) authInitArgs() []string {
	return []string{a.bin(), "run", "--rm", "--progress", "none", "--network", a.network(), "--user", "0", "--cap-drop", "ALL", "--cap-add", "CHOWN",
		"--cap-add", "FOWNER", "--mount", a.authMount(false), "--entrypoint", "sh", a.image(), "-c",
		"mkdir -p /rubi-auth/token && chown " + runnerUID + ":" + runnerUID + " /rubi-auth/token && chmod 700 /rubi-auth/token"}
}

// EnsureAuthVolume creates the auth volume and prepares it, unless it is there. A volume whose preparation
// failed is deleted again, so the next call starts over.
func (a *AppleContainer) EnsureAuthVolume(ctx context.Context) error {
	if _, err := a.do(ctx, []string{a.bin(), "volume", "inspect", a.volume()}); err == nil {
		return nil
	}
	if _, err := a.do(ctx, []string{a.bin(), "volume", "create", a.volume()}); err != nil {
		return err
	}
	if _, err := a.do(ctx, a.authInitArgs()); err != nil {
		_, _ = a.do(context.WithoutCancel(ctx), []string{a.bin(), "volume", "delete", a.volume()})
		return fmt.Errorf("preparing the sign-in volume: %w", err)
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

// statusScript prints SIGNED_IN (only that the token file isn't empty) and the metadata file, or NOT_SIGNED_IN.
const statusScript = `if [ -s /rubi-auth/token/oauth-token ]; then echo SIGNED_IN; cat /rubi-auth/token/oauth-token.meta 2>/dev/null; else echo NOT_SIGNED_IN; fi; exit 0`

// AuthStatus probes the volume in a throwaway container that mounts it read-only.
func (a *AppleContainer) AuthStatus(ctx context.Context) (AuthStatus, error) {
	if err := a.EnsureAuthVolume(ctx); err != nil {
		return AuthStatus{}, err
	}
	out, err := a.do(ctx, a.authSh(false, statusScript))
	if err != nil {
		return AuthStatus{}, err
	}
	return parseAuthStatus(string(out))
}

func parseAuthStatus(out string) (AuthStatus, error) {
	// The marker is the first line the script prints; anything the container tool printed before it is skipped.
	all := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range all {
		if t := strings.TrimSpace(l); t == "SIGNED_IN" || t == "NOT_SIGNED_IN" {
			all = all[i:]
			break
		}
	}
	lines := all
	if len(lines) > 2 {
		lines = []string{lines[0], strings.Join(lines[1:], "\n")}
	}
	switch strings.TrimSpace(lines[0]) {
	case "NOT_SIGNED_IN":
		return AuthStatus{}, nil
	case "SIGNED_IN":
	default:
		return AuthStatus{}, errors.New("unexpected output of the sign-in check")
	}
	st := AuthStatus{SignedIn: true}
	if len(lines) == 2 {
		var meta struct {
			Created string `json:"created"`
			Expires string `json:"expires"`
		}
		if json.Unmarshal([]byte(lines[1]), &meta) == nil {
			st.Created, _ = time.Parse(time.RFC3339, meta.Created)
			st.Expires, _ = time.Parse(time.RFC3339, meta.Expires)
		}
	}
	return st, nil
}

// AuthSignOut deletes the token and its metadata for every project (the next query of a running agent has none).
func (a *AppleContainer) AuthSignOut(ctx context.Context) error {
	if err := a.EnsureAuthVolume(ctx); err != nil {
		return err
	}
	_, err := a.do(ctx, a.authSh(true, "rm -f /rubi-auth/token/oauth-token /rubi-auth/token/oauth-token.meta"))
	return err
}

// AuthSession is the running sign-in container and its proxy.
type AuthSession struct {
	a     *AppleContainer
	px    *Proxy
	env   map[string]string
	ended bool
}

// StartAuth starts the sign-in container: no project, the volume read-write, the host-only network, and a
// proxy on the Mac's address on that network that allows only AuthHosts. It fails closed like a VM start.
func (a *AppleContainer) StartAuth(ctx context.Context) (*AuthSession, error) {
	if err := a.EnsureImage(ctx); err != nil {
		return nil, err
	}
	gateway, err := a.verifyInternal(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.EnsureAuthVolume(ctx); err != nil {
		return nil, err
	}
	del := []string{a.bin(), "delete", "--force", authContainer}
	_, _ = a.do(ctx, del) // a leftover of a crash
	run := []string{a.bin(), "run", "-d", "--progress", "none", "--name", authContainer, "--init", "--network", a.network(), "--user", runnerUID,
		"--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp", "--mount", a.authMount(false), "--entrypoint", "sleep", a.image(), "infinity"}
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

// Exec runs argv in the sign-in container with the proxy variables and extra. Its stderr is dropped.
func (s *AuthSession) Exec(ctx context.Context, argv []string, extra map[string]string) (Conn, error) {
	env := map[string]string{}
	for k, v := range s.env {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	return s.a.execNamed(ctx, authContainer, env, argv, false)
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
