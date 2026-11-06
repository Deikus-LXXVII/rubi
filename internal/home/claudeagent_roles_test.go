package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoleOpsThroughHandle(t *testing.T) {
	h, _, _ := agentHelper(t)
	if _, err := handle(t, h, "wrong", "claude-agent.role.list", nil); err == nil || !strings.Contains(err.Error(), "paired") {
		t.Fatalf("unpaired: %v", err)
	}
	// The project's legacy role is implicit.
	res, err := handle(t, h, "tok", "claude-agent.role.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	rows := res["roles"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["legacy"] != true || rows[0].(map[string]any)["id"] != "my-project" {
		t.Fatalf("%v", rows)
	}
	spec := map[string]any{"project": "my-project", "name": "visual", "description": "d", "tools": []string{"Read", "Grep"}}
	put, err := handle(t, h, "tok", "claude-agent.role.put", map[string]any{"base_version": 0, "spec": spec, "by": "bot-a"})
	if err != nil {
		t.Fatal(err)
	}
	id := put["id"].(string)
	if put["version"] != float64(1) {
		t.Fatalf("%v", put)
	}
	if tgt, err := os.Readlink(filepath.Join(h.Dir, "agent-roles", id, "content", "current")); err != nil || tgt != "v1" {
		t.Fatalf("%q %v", tgt, err)
	}
	spec["description"] = "e"
	if _, err := handle(t, h, "tok", "claude-agent.role.put", map[string]any{"id": id, "base_version": 0, "spec": spec}); err == nil || !strings.Contains(err.Error(), "base_version") {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := handle(t, h, "tok", "claude-agent.role.put", map[string]any{"id": id, "base_version": 1, "spec": spec}); err != nil {
		t.Fatal(err)
	}
	spec["project"] = "no-such"
	if _, err := handle(t, h, "tok", "claude-agent.role.put", map[string]any{"spec": spec}); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("%v", err)
	}
	got, err := handle(t, h, "tok", "claude-agent.role.get", map[string]any{"id": id, "version": 1})
	if err != nil || got["description"] != "d" {
		t.Fatalf("%v %v", got, err)
	}
	hist, err := handle(t, h, "tok", "claude-agent.role.history", map[string]any{"id": id, "limit": 5})
	if err != nil || len(hist["versions"].([]any)) != 2 {
		t.Fatalf("%v %v", hist, err)
	}
	d, err := handle(t, h, "tok", "claude-agent.role.diff", map[string]any{"id": id, "from": 1, "to": 2})
	if err != nil || len(d["changes"].([]any)) != 1 {
		t.Fatalf("%v %v", d, err)
	}
	res, _ = handle(t, h, "tok", "claude-agent.role.list", map[string]any{"project": "my-project"})
	if len(res["roles"].([]any)) != 2 {
		t.Fatalf("%v", res)
	}
}
