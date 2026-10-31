package setup

import (
	"context"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// ManagerAgents adapts the agent manager to Agents.
type ManagerAgents struct{ M *agent.Manager }

func (a ManagerAgents) Projects() []agent.Project { return a.M.Projects() }
func (a ManagerAgents) VMRunning(id string) bool  { return a.M.VMRunning(id) }

func (a ManagerAgents) Sandbox(ctx context.Context, id string) (Box, error) {
	return a.M.Sandbox(ctx, id)
}

func (a ManagerAgents) BeginSelfTest(id string) (SelfTestRun, error) {
	t, err := a.M.BeginSelfTest(id)
	if err != nil {
		return nil, err
	}
	return selfTest{t}, nil
}

type selfTest struct{ t *agent.SelfTest }

func (s selfTest) Sandbox(ctx context.Context) (Box, error) { return s.t.Sandbox(ctx) }
func (s selfTest) Turn(ctx context.Context, prompt string, d time.Duration) (agent.TurnResult, error) {
	return s.t.Turn(ctx, prompt, d)
}
func (s selfTest) End() { s.t.End() }
