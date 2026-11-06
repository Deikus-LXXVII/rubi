package setup

import (
	"context"
	"errors"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// ManagerAgents adapts the agent manager, and the VM backend for the sign-in, to Agents.
type ManagerAgents struct {
	M *agent.Manager
	// Login makes the host of the sign-in VM (the backend's image and runner sources).
	Login func() agent.LoginHost
	// SignInDir is the shared sign-in folder (<helper dir>/agent-signin).
	SignInDir func() string
}

func (a ManagerAgents) Projects() []agent.Project { return a.M.Projects() }

// SignedIn stats the credential file in the sign-in folder; it never opens it.
func (a ManagerAgents) SignedIn(context.Context) bool { return agent.SignedInAt(a.SignInDir()) }

func (a ManagerAgents) StartAuth(ctx context.Context) (AuthSession, error) {
	host := a.Login()
	if host == nil {
		return nil, errNoLogin
	}
	s, err := host.StartLogin(ctx, a.SignInDir())
	if err != nil {
		return nil, err
	}
	return s, nil
}

var errNoLogin = errors.New("signing in isn't available on this computer")
