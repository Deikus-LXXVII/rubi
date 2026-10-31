package home

import (
	"path/filepath"
	"strings"
	"testing"
)

// Only operations that start no command are called here (status would run the container tool).
func TestAgentSetupOpsThroughHandle(t *testing.T) {
	h, _, proj := agentHelper(t)
	for _, op := range []string{"status", "run", "login.code", "login.open", "project.add", "project.remove", "cancel"} {
		if _, err := handle(t, h, "wrong", "agent-setup."+op, map[string]any{}); err == nil || !strings.Contains(err.Error(), "paired") {
			t.Fatalf("%s unpaired: %v", op, err)
		}
	}
	hello, _ := handle(t, h, "tok", "hello", nil)
	found := false
	for _, c := range hello["can"].([]any) {
		found = found || c == "agent-setup"
	}
	if !found {
		t.Fatalf("hello: %v", hello)
	}
	if _, err := handle(t, h, "tok", "agent-setup.run", map[string]any{"step": "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("unknown step: %v", err)
	}
	if res, err := handle(t, h, "tok", "agent-setup.cancel", nil); err != nil || res["stopped"] != false {
		t.Errorf("cancel: %v %v", res, err)
	}
	if _, err := handle(t, h, "tok", "agent-setup.login.code", map[string]any{"project": "my-project", "code": "abc"}); err == nil {
		t.Error("a code with no sign-in")
	}
	// project.remove goes through the helper's own configuration
	if _, err := handle(t, h, "tok", "agent-setup.project.remove", map[string]any{"dir": filepath.Join(proj, "nope")}); err == nil {
		t.Error("removed a non-project")
	}
	real, _ := filepath.EvalSymlinks(proj)
	if res, err := handle(t, h, "tok", "agent-setup.project.remove", map[string]any{"dir": real}); err != nil || res["removed"] != true {
		t.Fatalf("remove: %v %v", res, err)
	}
	if got := h.Fresh().AgentProjects; len(got) != 0 {
		t.Errorf("agent_projects: %v", got)
	}
}
