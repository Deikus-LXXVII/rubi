package t3instances

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
)

func TestSandboxInstancesRouteEverySupportedDriverAndPreservePersonalInstances(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	os.Mkdir(a, 0700)
	os.Mkdir(b, 0700)
	o := InstanceOptions{T3Base: filepath.Join(root, "t3"), Dir: filepath.Join(root, "rubi"), Projects: []T3Project{{"a", a}, {"b", b}}}
	path := o.settingsPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	personal := `{"providerInstances":{"personal":{"driver":"cursor","enabled":true,"config":{}}},"other":123}`
	os.WriteFile(path, []byte(personal), 0600)
	recorder := filepath.Join(root, "record args")
	os.WriteFile(recorder, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700)
	role := SandboxRole{ID: "r_aaaaaa01", Name: "Role", Project: "a", ProjectDir: a, ClaudeDir: filepath.Join(o.Dir, "role-login")}
	res, err := SyncSandboxInstances(o, recorder, []SandboxRole{role})
	if err != nil || !res.Changed() {
		t.Fatalf("sync: %+v %v", res, err)
	}
	raw, _ := os.ReadFile(path)
	var top struct {
		Other             int
		ProviderInstances map[string]struct {
			Driver  string
			Enabled bool
			Config  map[string]any
		}
		ProjectSettingsOverrides map[string]struct {
			ProviderInstances map[string]struct{ Enabled bool }
		}
	}
	if err = json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if top.Other != 123 || !top.ProviderInstances["personal"].Enabled {
		t.Fatal("personal settings changed")
	}
	for _, route := range agentsandbox.ProviderRoutes {
		id := sandboxInstanceID(role.ID, route.Driver)
		entry, ok := top.ProviderInstances[id]
		if route.BinaryField == "" {
			if ok {
				t.Fatal("SDK/HTTP provider exposed without confinement")
			}
			continue
		}
		if !ok || entry.Driver != route.Driver || entry.Enabled || !top.ProjectSettingsOverrides["a"].ProviderInstances[id].Enabled || top.ProjectSettingsOverrides["b"].ProviderInstances[id].Enabled {
			t.Fatalf("wrong project switch for %s", id)
		}
		shim := entry.Config[route.BinaryField].(string)
		cmd := exec.Command(shim, "--config", "untrusted", "a b", "$(false)")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 7 || lines[0] != "--config" || lines[2] != "--" || lines[3] != "--config" || lines[4] != "untrusted" || lines[5] != "a b" || lines[6] != "$(false)" {
			t.Fatalf("argument boundary lost: %q", out)
		}
		var binding agentsandbox.Binding
		raw, _ := os.ReadFile(lines[1])
		if json.Unmarshal(raw, &binding) != nil || binding.Project != "a" || binding.Role != role.ID {
			t.Fatal("shim is not bound to role/project")
		}
	}
	// User/T3 additions to managed config survive, while launch controls stay fixed.
	var settings map[string]json.RawMessage
	json.Unmarshal(raw, &settings)
	var instances map[string]map[string]json.RawMessage
	json.Unmarshal(settings["providerInstances"], &instances)
	claude := instances[InstancePrefix+role.ID]
	claude["accentColor"] = jstr("purple")
	var cfg map[string]json.RawMessage
	json.Unmarshal(claude["config"], &cfg)
	cfg["launchArgs"] = jstr("--verbose")
	claude["config"] = mustMarshal(cfg)
	settings["providerInstances"] = mustMarshal(instances)
	os.WriteFile(path, mustMarshal(settings), 0600)
	if res, err := SyncSandboxInstances(o, recorder, []SandboxRole{role}); err != nil || res.Changed() {
		t.Fatalf("not idempotent: %+v %v", res, err)
	}
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "--verbose") || !strings.Contains(string(raw), "purple") {
		t.Fatal("T3 additions lost")
	}
	// Stale project overrides get turned off when the project disappears from T3's list.
	o.Projects = []T3Project{{"b", b}}
	if _, err := SyncSandboxInstances(o, recorder, []SandboxRole{role}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	json.Unmarshal(raw, &top)
	if top.ProjectSettingsOverrides["a"].ProviderInstances[InstancePrefix+role.ID].Enabled {
		t.Fatal("stale project still enabled")
	}
}

func TestSandboxInstancesRefuseForeignManagedIDAndBadSettings(t *testing.T) {
	for _, raw := range []string{`{"projectSettingsOverrides":[]}`, `{"providerInstances":{"claude-r_aaaaaa01":{"driver":"claudeAgent","config":{"binaryPath":"/user/claude"}}}}`} {
		root := t.TempDir()
		o := InstanceOptions{Dir: filepath.Join(root, "rubi"), T3Base: filepath.Join(root, "t3")}
		os.MkdirAll(filepath.Dir(o.settingsPath()), 0700)
		os.WriteFile(o.settingsPath(), []byte(raw), 0600)
		if _, err := SyncSandboxInstances(o, "/usr/bin/true", []SandboxRole{{ID: "r_aaaaaa01"}}); err == nil {
			t.Fatal("unsafe settings accepted")
		}
		after, _ := os.ReadFile(o.settingsPath())
		if string(after) != raw {
			t.Fatal("settings changed on refusal")
		}
	}
}

func TestSandboxInstancesHandleNullMapsAndRejectNullSettings(t *testing.T) {
	for _, raw := range []string{`null`, `{"providerInstances":null,"projectSettingsOverrides":null}`} {
		root := t.TempDir()
		o := InstanceOptions{Dir: filepath.Join(root, "rubi"), T3Base: filepath.Join(root, "t3")}
		os.MkdirAll(filepath.Dir(o.settingsPath()), 0700)
		os.WriteFile(o.settingsPath(), []byte(raw), 0600)
		_, err := SyncSandboxInstances(o, "/usr/bin/true", []SandboxRole{{ID: "r_aaaaaa01", ClaudeDir: filepath.Join(root, "login")}})
		if (err != nil) != (raw == "null") {
			t.Fatalf("null handling: %s %v", raw, err)
		}
	}
}

func TestSandboxInstancesRemoveOrphanedProjectSwitch(t *testing.T) {
	root := t.TempDir()
	o := InstanceOptions{Dir: filepath.Join(root, "rubi"), T3Base: filepath.Join(root, "t3")}
	os.MkdirAll(filepath.Dir(o.settingsPath()), 0700)
	os.WriteFile(o.settingsPath(), []byte(`{"providerInstances":{},"projectSettingsOverrides":{"gone":{"providerInstances":{"claude-r_aaaaaa01":{"enabled":true},"personal":{"enabled":true}}}}}`), 0600)
	if _, err := SyncSandboxInstances(o, "/usr/bin/true", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(o.settingsPath())
	if strings.Contains(string(raw), "claude-r_aaaaaa01") || !strings.Contains(string(raw), "personal") {
		t.Fatal("orphan switches not cleaned selectively")
	}
}

func TestManagedOpenCodeCannotUseRemoteURLOrWeakAuthentication(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	os.Mkdir(project, 0700)
	o := InstanceOptions{Dir: filepath.Join(root, "rubi"), T3Base: filepath.Join(root, "t3")}
	role := SandboxRole{ID: "r_aaaaaa01", Project: "project", ProjectDir: project, ClaudeDir: filepath.Join(root, "login")}
	if _, err := SyncSandboxInstances(o, "/bin/true", []SandboxRole{role}); err != nil {
		t.Fatal(err)
	}
	path := o.settingsPath()
	raw, _ := os.ReadFile(path)
	var settings map[string]json.RawMessage
	json.Unmarshal(raw, &settings)
	var instances map[string]map[string]json.RawMessage
	json.Unmarshal(settings["providerInstances"], &instances)
	entry := instances[sandboxInstanceID(role.ID, "opencode")]
	var cfg map[string]any
	json.Unmarshal(entry["config"], &cfg)
	password, _ := cfg["serverPassword"].(string)
	if len(password) < 32 {
		t.Fatal("strong bridge authentication not provisioned")
	}
	cfg["serverUrl"] = "http://untrusted.invalid"
	cfg["serverPassword"] = "weak"
	entry["config"] = mustMarshal(cfg)
	settings["providerInstances"] = mustMarshal(instances)
	os.WriteFile(path, mustMarshal(settings), 0600)
	if _, err := SyncSandboxInstances(o, "/bin/true", []SandboxRole{role}); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	json.Unmarshal(raw, &settings)
	json.Unmarshal(settings["providerInstances"], &instances)
	json.Unmarshal(instances[sandboxInstanceID(role.ID, "opencode")]["config"], &cfg)
	password, _ = cfg["serverPassword"].(string)
	if cfg["serverUrl"] != "" || len(password) < 32 {
		t.Fatal("remote transport or weak authentication survived sync")
	}
	if res, err := SyncSandboxInstances(o, "/bin/true", []SandboxRole{role}); err != nil || res.Changed() {
		t.Fatal("bridge credentials rotated unnecessarily")
	}
}

// Native instances retain the concurrent-settings protection formerly tested only via VM shims.
func TestSandboxInstancesKeepConcurrentPersonalSettings(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	os.Mkdir(project, 0700)
	o := InstanceOptions{T3Base: filepath.Join(root, "t3"), Dir: filepath.Join(root, "rubi"), Projects: []T3Project{{"project", project}}}
	helper := filepath.Join(root, "helper")
	os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700)
	path := o.settingsPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(`{"theme":"dark"}`), 0600)
	n := 0
	beforeRename = func() {
		n++
		if n == 1 {
			os.WriteFile(path, []byte(`{"theme":"light","providerInstances":{"personal":{"driver":"codex"}}}`), 0600)
		}
	}
	defer func() { beforeRename = nil }()
	_, err := SyncSandboxInstances(o, helper, []SandboxRole{{ID: "r_aaaaaa01", Name: "Role", Project: "project", ProjectDir: project, ClaudeDir: filepath.Join(o.Dir, "login")}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if top["theme"] != "light" || top["providerInstances"].(map[string]any)["personal"] == nil || n != 2 {
		t.Fatal("concurrent personal settings were lost")
	}
}
