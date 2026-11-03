package home

// The sign-in of the T3 roles: one `claude auth login --claudeai` for every role (decision 4 of
// docs/plans/t3-integration.md). It runs in a sign-in container (no project; the shared folder <helper>/agent-signin
// mounted read-write at its own path as CLAUDE_SECURESTORAGE_CONFIG_DIR; the network only through the allow-list proxy
// for the sign-in hosts), driven by the same job engine as the agent setup's sign-in (setup.Setup: the helper's link and
// code prompt, the code relay, cancel). Home never reads the credential claude writes; "signed in" is the file being
// there and not empty.

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

// t3LoginAgents is the part of setup.Agents the T3 sign-in uses.
type t3LoginAgents struct{ t *T3Home }

func (t3LoginAgents) Projects() []agent.Project { return nil }
func (t3LoginAgents) BeginSelfTest(string) (setup.SelfTestRun, error) {
	return nil, errors.New("the T3 sign-in has no self-test of its own")
}

func (a t3LoginAgents) AuthStatus(context.Context) (agent.AuthStatus, error) {
	return agent.AuthStatus{SignedIn: agent.SignedInAt(a.t.signInDir())}, nil
}

// AuthSignOut deletes the shared credential file (it can't revoke it: that is done at claude.ai).
func (a t3LoginAgents) AuthSignOut(context.Context) error {
	err := os.Remove(filepath.Join(a.t.signInDir(), agent.CredentialFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a t3LoginAgents) StartAuth(ctx context.Context) (setup.AuthSession, error) {
	c := a.t.h.Fresh()
	ac := &agent.AppleContainer{RunnerDir: c.AgentRunnerDir, EnvDir: a.t.h.Dir, AuthDir: c.AgentAuthDir}
	s, err := ac.StartLogin(ctx, a.t.signInDir())
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (t *T3Home) signInDir() string { return agent.SignInDirOf(t.h.Dir) }

// login is the sign-in job engine.
func (t *T3Home) login() *setup.Setup {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loginSetup == nil {
		var agents setup.Agents = t3LoginAgents{t}
		if t.LoginAgents != nil {
			agents = t.LoginAgents
		}
		opt := setup.Options{Dir: t.h.Dir, Agents: agents, LoginHelper: agent.LoginHelper,
			LoginEnv: func() map[string]string {
				dir := t.signInDir()
				if real, err := agent.EnsureSignInDir(dir); err == nil {
					dir = real
				}
				return agent.LoginEnv(dir)
			}}
		if t.LoginOptions != nil {
			t.LoginOptions(&opt)
		}
		t.loginSetup = setup.New(opt)
	}
	return t.loginSetup
}

// startLogin is t3-setup.run {step: "login", method: "claudeai"}. role names a role (it is checked, and shown in the
// status); the sign-in itself is the one shared by every role.
func (t *T3Home) startLogin(role, method string) error {
	if role != "" {
		known := false
		for _, r := range t.h.Agent.T3Projects() {
			known = known || r.Role == role
		}
		if !known {
			return errors.New("unknown role")
		}
	}
	return t.login().StartLogin(method)
}
