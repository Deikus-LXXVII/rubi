package home

import (
	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/agenttest"
)

// The tests of the operations run on every system and exercise Home's own logic (agents, sessions, shortcuts with a fake
// runner), so they run as on a Mac, where every feature is available. What each system really offers is tested in
// features_test.go, which names the system and its Desktop itself and never uses this default.
func init() {
	systemFeatureEnv = func() FeatureEnv {
		return FeatureEnv{GOOS: "darwin", Backend: agenttest.NewAppleBackend(agent.BackendConfig{})}
	}
}
