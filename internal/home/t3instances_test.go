package home

import (
	"encoding/json"
	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncT3InstancesWritesTheRolesInstances(t *testing.T) {
	h, _, _ := agentHelper(t)
	spec := map[string]any{"project": "my-project", "name": "reviewer", "description": "d", "tools": []string{"Read"}}
	put, err := handle(t, h, "tok", "claude-agent.role.put", map[string]any{"base_version": 0, "spec": spec, "by": "bot-a"})
	if err != nil {
		t.Fatal(err)
	}
	id := put["id"].(string)
	base := filepath.Join(t.TempDir(), "t3")
	if err := h.Update(func(c *Config) { c.T3Port, c.T3BaseDir = 4411, base }); err != nil {
		t.Fatal(err)
	}
	res, err := h.SyncT3Instances()
	n := 0
	for _, route := range agentsandbox.ProviderRoutes {
		if route.BinaryField != "" {
			n++
		}
	}
	if err != nil || len(res.Added) != n || res.Added[0] != "claude-"+id {
		t.Fatalf("%+v %v", res, err)
	}
	b, err := os.ReadFile(filepath.Join(base, "userdata", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		ProviderInstances map[string]struct {
			Driver, DisplayName string
			Config              struct{ BinaryPath, HomePath string }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	in := s.ProviderInstances["claude-"+id]
	if in.Driver != "claudeAgent" || in.DisplayName != "reviewer · claudeAgent" || in.Config.HomePath != filepath.Join(h.Dir, "agent-roles", id, "claude") ||
		in.Config.BinaryPath != filepath.Join(h.Dir, "t3", "bin", "claude-"+id) {
		t.Fatalf("instance: %+v", in)
	}
}
