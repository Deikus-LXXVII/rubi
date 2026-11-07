package panelapi_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
)

// A setup link opens that plugin's own page, and the view-only settings an approval link
// opens can read the page. A write from that view-only session is still refused.
func TestPluginPageFromSetupAndApprovalLinks(t *testing.T) {
	c, _ := setup(t)
	pc := client(t, c, "pair")
	if _, err := pc.PairWithPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	installViewPlugin(t, c)

	setupLink := client(t, c, "setup:notesbox")
	err := setupLink.Call("plugin.panel", map[string]any{"id": "notesbox", "op": "view", "args": map[string]any{"page": "setup"}}, nil)
	if err == nil || strings.Contains(err.Error(), "settings link") || strings.Contains(err.Error(), "only sets up") {
		t.Fatalf("setup link could not open the plugin page: %v", err)
	}
	err = setupLink.Call("plugin.panel", map[string]any{"id": "other", "op": "view"}, nil)
	if err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("setup link opened another plugin: %v", err)
	}

	view := client(t, c, "approve:apr_x")
	var vs struct {
		Ticket   string `json:"ticket"`
		ViewOnly bool   `json:"view_only"`
	}
	if err := view.Call("session.settings", nil, &vs); err != nil || !vs.ViewOnly || vs.Ticket == "" {
		t.Fatalf("session.settings: %+v %v", vs, err)
	}
	view.Link.Ticket = vs.Ticket
	err = view.Call("plugin.panel", map[string]any{"id": "notesbox", "op": "view", "args": map[string]any{}}, nil)
	if err == nil || strings.Contains(err.Error(), "settings link") {
		t.Fatalf("view-only settings could not read the plugin page: %v", err)
	}
	err = view.Call("plugin.panel", map[string]any{"id": "notesbox", "op": "save", "args": map[string]any{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "settings link") {
		t.Fatalf("view-only settings changed the plugin page: %v", err)
	}

	var cat struct {
		Integrations []struct {
			ID    string `json:"id"`
			Panel string `json:"panel"`
		} `json:"integrations"`
	}
	if err := setupLink.Call("integration.catalog", nil, &cat); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range cat.Integrations {
		if it.ID == "notesbox" {
			found = it.Panel == "view"
		}
	}
	if !found {
		t.Fatalf("catalog did not name the plugin page: %+v", cat.Integrations)
	}
}

func installViewPlugin(t *testing.T, c *core.Core) {
	t.Helper()
	const raw = `{"schema":1,"id":"notesbox","name":"Notes Box","version":"v1.0.0","api":1,"entry":"notesbox","panel":"view",
		"publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="}}`
	var m plugins.Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if err := plugins.Check(&m); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notesbox"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree, err := plugins.TreeHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Store.Commit(&plugins.Candidate{Manifest: m, Dir: dir, Tree: tree}, ""); err != nil {
		t.Fatal(err)
	}
}
