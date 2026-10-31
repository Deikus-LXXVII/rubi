package setup

import (
	"context"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// ManagerAgents adapts the agent manager, and the container tool for the shared sign-in, to Agents.
type ManagerAgents struct {
	M *agent.Manager
	// Auth makes the container definition the shared sign-in uses (network, image, volume).
	Auth func() *agent.AppleContainer
}

func (a ManagerAgents) Projects() []agent.Project { return a.M.Projects() }

func (a ManagerAgents) BeginSelfTest(id string) (SelfTestRun, error) {
	t, err := a.M.BeginSelfTest(id)
	if err != nil {
		return nil, err
	}
	return selfTest{t}, nil
}

func (a ManagerAgents) AuthStatus(ctx context.Context) (agent.AuthStatus, error) {
	return a.Auth().AuthStatus(ctx)
}

func (a ManagerAgents) AuthSignOut(ctx context.Context) error { return a.Auth().AuthSignOut(ctx) }

func (a ManagerAgents) StartAuth(ctx context.Context) (AuthSession, error) {
	s, err := a.Auth().StartAuth(ctx)
	if err != nil {
		return nil, err
	}
	return s, nil
}

type selfTest struct{ t *agent.SelfTest }

func (s selfTest) Sandbox(ctx context.Context) (Box, error) { return s.t.Sandbox(ctx) }
func (s selfTest) Turn(ctx context.Context, prompt string, d time.Duration) (agent.TurnResult, error) {
	return s.t.Turn(ctx, prompt, d)
}
func (s selfTest) End() { s.t.End() }
