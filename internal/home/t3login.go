package home

// The sign-in of the T3 roles: one `claude auth login --claudeai` for every role (decision 4 of
// docs/plans/t3-integration.md). It runs in a sign-in container (no project; the shared folder <helper>/agent-signin
// mounted read-write at its own path as CLAUDE_SECURESTORAGE_CONFIG_DIR; the network only through the allow-list proxy
// for the sign-in hosts), driven by the same job engine as the agent setup's sign-in (setup.Setup: the helper's link and
// code prompt, the code relay, cancel). Home never reads the credential claude writes; "signed in" is the file being
// there and not empty.

import (
	"errors"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

func (t *T3Home) signInDir() string { return agent.SignInDirOf(t.h.Dir) }

// login is the sign-in job engine.
func (t *T3Home) login() *setup.Setup {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loginSetup == nil {
		var agents setup.Agents = setup.ManagerAgents{M: t.h.Agent, SignInDir: t.signInDir, Login: func() agent.LoginHost {
			return t.h.backend().Login()
		}}
		if t.LoginAgents != nil {
			agents = t.LoginAgents
		}
		opt := setup.Options{Dir: t.h.Dir, Agents: agents, Backend: t.h.backend, LoginHelper: agent.LoginHelper, Opener: NewDesktop(DesktopEnv{}).Opener,
			LoginEnv: func() map[string]string {
				dir := t.signInDir()
				if real, err := agent.EnsureSignInDir(dir); err == nil {
					dir = real
				}
				return agent.LoginEnv(dir)
			},
			// The code handler only writes the code. When the helper then saves the credential,
			// this starts the self-test. A panel does not have to.
			OnSignedIn: func() { setup.AfterSignIn(func() error { return t.RunStepFor(T3StepSelfTest, "", "") }) },
		}
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
