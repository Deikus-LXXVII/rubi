package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	_ "embed"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

//go:embed loginhelper/rubi-claude-login
var loginHelperSource []byte

// nativeLoginHelper is the name passed to setup.StartLogin; the real helper is
// prepared at StartAuth and is the process the sandbox runs.
const nativeLoginHelper = "rubi-claude-login"

// providerSignInHosts is the network allow-list for native Claude sign-in
// (docs/plans/t3-integration.md, S3 part 2). api.anthropic.com is for the
// token exchange; a sign-in *link* may not point there (see setup loginHosts).
var providerSignInHosts = []string{
	"claude.com", "claude.ai", "platform.claude.com", "console.anthropic.com", "api.anthropic.com",
}

func nativeLoginEnv(signInDir string) map[string]string {
	return map[string]string{
		"CLAUDE_SECURESTORAGE_CONFIG_DIR": signInDir,
		"CLAUDE_CONFIG_DIR":               signInDir,
		"BROWSER":                         "false",
		"NO_COLOR":                        "1",
		"TERM":                            "xterm-256color",
	}
}

// nativeLoginAgents is the default Agents for T3 sign-in on the native (srt) path.
type nativeLoginAgents struct{ t *T3Home }

func (a nativeLoginAgents) Projects() []agent.Project {
	if a.t == nil || a.t.h == nil || a.t.h.Agent == nil {
		return nil
	}
	return a.t.h.Agent.Projects()
}

func (a nativeLoginAgents) SignedIn(context.Context) bool {
	return agent.SignedInAt(a.t.signInDir())
}

func (a nativeLoginAgents) StartAuth(ctx context.Context) (setup.AuthSession, error) {
	return a.t.startNativeAuth(ctx)
}

type nativeAuth struct {
	t      *T3Home
	launch agentsandbox.Launch
	cancel context.CancelFunc
}

func (t *T3Home) startNativeAuth(ctx context.Context) (setup.AuthSession, error) {
	cli, err := t.claudeCLI()
	if err != nil {
		return nil, err
	}
	helper, err := t.prepareLoginHelper(cli)
	if err != nil {
		return nil, err
	}
	launch, err := t.nativeLoginLaunch(helper, cli)
	if err != nil {
		return nil, err
	}
	return &nativeAuth{t: t, launch: launch}, nil
}

func (t *T3Home) claudeCLI() (string, error) {
	if t.h == nil {
		return "", errors.New("Claude is not installed on this computer yet")
	}
	cli := t.h.Fresh().AgentSandbox.Providers["claudeAgent"].CLI
	if cli == "" {
		return "", errors.New("Claude is not installed on this computer yet")
	}
	st, err := os.Stat(cli)
	if err != nil || st.IsDir() || st.Mode()&0111 == 0 {
		return "", errors.New("Claude is not installed on this computer yet")
	}
	return cli, nil
}

var claudeConstRE = regexp.MustCompile(`const CLAUDE = "[^"]*";`)

func (t *T3Home) prepareLoginHelper(claudePath string) (string, error) {
	quoted, err := json.Marshal(claudePath)
	if err != nil {
		return "", err
	}
	src := claudeConstRE.ReplaceAll(loginHelperSource, []byte("const CLAUDE = "+string(quoted)+";"))
	if !bytes.Contains(src, append([]byte("const CLAUDE = "), quoted...)) {
		return "", errors.New("the sign-in helper could not be prepared")
	}
	if runtime.GOOS == "darwin" {
		src = bytes.Replace(src,
			[]byte(`cp.spawn("script", ["-qfc", inner, "/dev/null"]`),
			[]byte(`cp.spawn("script", ["-q", "/dev/null", "/bin/bash", "-c", inner]`),
			1)
	}
	dir := filepath.Join(t.root(), "login-helper")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, nativeLoginHelper)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, src, 0700); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// nativeLoginLaunch is the srt policy for the sign-in: only the shared login
// folder (and a throwaway tmp and empty workspace) are writable; the network is
// only the sign-in hosts; no project folder is mounted.
func (t *T3Home) nativeLoginLaunch(helper, claude string) (agentsandbox.Launch, error) {
	var l agentsandbox.Launch
	login, err := agent.EnsureSignInDir(t.signInDir())
	if err != nil {
		return l, err
	}
	run := filepath.Join(t.root(), "login-run")
	workspace := filepath.Join(run, "workspace")
	home := filepath.Join(run, "home")
	tmp := filepath.Join(run, "tmp")
	settings := filepath.Join(run, "settings")
	for _, d := range []string{workspace, home, tmp, settings, filepath.Dir(helper)} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return l, err
		}
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return l, errors.New("the sign-in folders must be private and not symlinks")
		}
		if err := os.Chmod(d, 0700); err != nil {
			return l, err
		}
	}
	install := agentsandbox.Installation{}
	if t.h != nil {
		install = t.h.Fresh().AgentSandbox
	}
	if install.SRT == "" || len(install.SearchPath) == 0 {
		return l, errors.New("the sandbox runtime is not installed on this computer yet")
	}
	sensitive := []string{t.h.Dir}
	if host, err := os.UserHomeDir(); err == nil {
		sensitive = append(sensitive, host)
	}
	if t.h.Agent != nil {
		for _, p := range t.h.Agent.Projects() {
			if p.Dir != "" {
				sensitive = append(sensitive, p.Dir)
			}
		}
	}
	runtimeRead := append([]string{}, install.RuntimeRead...)
	runtimeRead = append(runtimeRead, filepath.Dir(helper), filepath.Dir(claude))
	l = agentsandbox.Launch{
		Policy: agentsandbox.Policy{
			Projects:       []string{workspace},
			Home:           home,
			Login:          login,
			Tmp:            tmp,
			SensitiveRoots: sensitive,
			RuntimeRead:    runtimeRead,
			AllowedDomains: append([]string{}, providerSignInHosts...),
		},
		SettingsDir: settings,
		SRT:         install.SRT,
		CLI:         helper,
		SearchPath:  install.SearchPath,
		Provider:    "claudeAgent",
	}
	for _, p := range []string{filepath.Join(t.h.Dir, "config.json"), filepath.Join(t.h.Dir, agent.RolesDir), filepath.Join(t.root(), "bin")} {
		if _, e := os.Lstat(p); e == nil {
			l.Protected = append(l.Protected, p)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return l, err
	}
	l, err = agentsandbox.ResolveRuntime(l, self)
	if err != nil {
		return l, fmt.Errorf("the sandbox runtime is not ready: %w", err)
	}
	return l, nil
}

func (s *nativeAuth) Exec(ctx context.Context, _ []string, _ map[string]string) (agent.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	stdoutR, stdoutW := io.Pipe()
	stdinR, stdinW := io.Pipe()
	c := &nativeConn{r: stdoutR, w: stdinW, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer stdoutW.Close()
		defer stdinR.Close()
		_, err := agentsandbox.Run(ctx, s.launch, stdinR, stdoutW, io.Discard)
		c.mu.Lock()
		c.runErr = err
		c.mu.Unlock()
	}()
	return c, nil
}

func (s *nativeAuth) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}

type nativeConn struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	runErr error
	once   sync.Once
}

func (c *nativeConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *nativeConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *nativeConn) CloseWrite() error           { return c.w.Close() }
func (c *nativeConn) Done() <-chan struct{}       { return c.done }
func (c *nativeConn) Close() error {
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		_ = c.w.Close()
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			_ = c.r.Close()
			<-c.done
		}
	})
	return nil
}
