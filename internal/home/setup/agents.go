package setup

import (
	"context"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// ManagerAgents adapts the agent manager, and the container tool for the sign-in, to Agents.
type ManagerAgents struct {
	M *agent.Manager
	// Auth makes the container definition the sign-in uses (image, runner sources).
	Auth func() *agent.AppleContainer
	// SignInDir is the shared sign-in folder (<helper dir>/agent-signin).
	SignInDir func() string
}

func (a ManagerAgents) Projects() []agent.Project { return a.M.Projects() }

// SignedIn stats the credential file in the sign-in folder; it never opens it.
func (a ManagerAgents) SignedIn(context.Context) bool { return agent.SignedInAt(a.SignInDir()) }

func (a ManagerAgents) StartAuth(ctx context.Context) (AuthSession, error) {
	s, err := a.Auth().StartLogin(ctx, a.SignInDir())
	if err != nil {
		return nil, err
	}
	return s, nil
}
