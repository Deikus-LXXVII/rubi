package t3vm

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func instFixture(t *testing.T, settings string) (InstanceOptions, string) {
	t.Helper()
	base := t.TempDir()
	o := InstanceOptions{T3Base: filepath.Join(base, "t3"), Dir: filepath.Join(base, "helper"), RubiHome: filepath.Join(base, "bin dir", "rubi-home")}
	path := filepath.Join(o.T3Base, "userdata", "settings.json")
	if settings != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return o, path
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return m
}

func instances(m map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, v := range m["providerInstances"].(map[string]any) {
		out[k] = v.(map[string]any)
	}
	return out
}

func TestSyncInstancesWritesOneInstanceAndShimPerRole(t *testing.T) {
	o, path := instFixture(t, "")
	want := []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/agent-roles/r_aaaaaa01/claude", ""}, {"r_aaaaaa02", "Builder's", "/h/agent-roles/r_aaaaaa02/claude", ""}}
	res, err := SyncInstances(o, want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Added, []string{"claude-r_aaaaaa01", "claude-r_aaaaaa02"}) || !res.Changed() {
		t.Fatalf("result: %+v", res)
	}
	in := instances(readSettings(t, path))
	e := in["claude-r_aaaaaa01"]
	cfg := e["config"].(map[string]any)
	if e["driver"] != "claudeAgent" || e["displayName"] != "Reviewer" || e["enabled"] != false ||
		cfg["binaryPath"] != o.ShimPath("r_aaaaaa01") || cfg["homePath"] != "/h/agent-roles/r_aaaaaa01/claude" || e["environment"] != nil {
		t.Fatalf("instance: %v", e)
	}
	fi, err := os.Stat(o.ShimPath("r_aaaaaa01"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("shim: %v %v", fi, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings mode %v", fi.Mode())
	}
}

// The shim is run by T3 as the binary: it must exec rubi-home with the role and pass T3's arguments through, whatever
// is in the paths.
func TestShimRunsRubiHomeWithTheRoleAndTheArguments(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "it's a dir", "rubi-home")
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$RUBI_HOME_DIR\" > " + shQuote(out) + "\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> " + shQuote(out) + "\n"
	if err := os.WriteFile(home, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	helperDir := filepath.Join(dir, "it's home")
	script, err := ShimScript(home, helperDir, "r_aaaaaa01")
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "claude-r_aaaaaa01")
	if err := os.WriteFile(shim, script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(shim, "--model", "a b", "$(touch pwned)", "--x=y z").Run(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); !reflect.DeepEqual(got, []string{helperDir, "agent-vm", "--role", "r_aaaaaa01", "--", "--model", "a b", "$(touch pwned)", "--x=y z"}) {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat("pwned"); err == nil {
		t.Fatal("an argument was run as a command")
	}
	for _, bad := range []struct{ home, dir, role string }{{"relative/rubi-home", helperDir, "r_aaaaaa01"}, {"/a\nb/rubi-home", helperDir, "r_aaaaaa01"},
		{home, "relative", "r_aaaaaa01"}, {home, "/a\nb", "r_aaaaaa01"}, {home, helperDir, "r_aaaaaa01; rm -rf /"}, {home, helperDir, ""}} {
		if _, err := ShimScript(bad.home, bad.dir, bad.role); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestSyncInstancesKeepsEverythingItDidNotWrite(t *testing.T) {
	o, path := instFixture(t, `{"theme":"dark","providerInstances":{
		"claude-mine":{"driver":"claudeAgent","displayName":"Mine","config":{"binaryPath":"/usr/local/bin/claude"},"extra":{"a":1}},
		"codex":{"driver":"codex","config":{}}},"other":[1,2]}`)
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	if m["theme"] != "dark" || !reflect.DeepEqual(m["other"], []any{1.0, 2.0}) {
		t.Fatalf("other settings lost: %v", m)
	}
	in := instances(m)
	if len(in) != 3 || in["claude-mine"]["displayName"] != "Mine" || in["claude-mine"]["extra"] == nil || in["codex"]["driver"] != "codex" {
		t.Fatalf("instances: %v", in)
	}
	// and when the role is gone, only Home's instance goes
	res, err := SyncInstances(o, nil)
	if err != nil || !reflect.DeepEqual(res.Removed, []string{"claude-r_aaaaaa01"}) {
		t.Fatalf("%+v %v", res, err)
	}
	in = instances(readSettings(t, path))
	if len(in) != 2 || in["claude-mine"] == nil || in["codex"] == nil {
		t.Fatalf("after removal: %v", in)
	}
	if _, err := os.Stat(o.ShimPath("r_aaaaaa01")); err == nil {
		t.Error("the shim of a deleted role is still there")
	}
}

func TestSyncInstancesLeavesAForeignInstanceWithARolesIDAlone(t *testing.T) {
	o, path := instFixture(t, `{"providerInstances":{"claude-r_aaaaaa01":{"driver":"claudeAgent","displayName":"Theirs","config":{"binaryPath":"/usr/local/bin/claude","homePath":"/x"}}}}`)
	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}})
	if err != nil || !reflect.DeepEqual(res.Skipped, []string{"claude-r_aaaaaa01"}) || res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	e := instances(readSettings(t, path))["claude-r_aaaaaa01"]
	if e["displayName"] != "Theirs" || e["config"].(map[string]any)["binaryPath"] != "/usr/local/bin/claude" {
		t.Fatalf("the user's instance was changed: %v", e)
	}
	if _, err := os.Stat(o.ShimPath("r_aaaaaa01")); err == nil {
		t.Error("a shim was written for an instance that is not Home's")
	}
}

func TestSyncInstancesUpdatesWhatChangedAndKeepsT3sAdditions(t *testing.T) {
	o, path := instFixture(t, "")
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}}); err != nil {
		t.Fatal(err)
	}
	// T3 (or the user) adds fields to the entry; the user also tries an environment
	m := readSettings(t, path)
	e := instances(m)["claude-r_aaaaaa01"]
	e["accentColor"] = "#fff"
	e["environment"] = []any{map[string]any{"name": "ANTHROPIC_API_KEY", "value": "sk-x"}}
	e["enabled"] = false
	m["providerInstances"].(map[string]any)["claude-r_aaaaaa01"] = e
	b, _ := json.Marshal(m)
	os.WriteFile(path, b, 0o600)

	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer v2", "/h/c2", ""}})
	if err != nil || !reflect.DeepEqual(res.Updated, []string{"claude-r_aaaaaa01"}) {
		t.Fatalf("%+v %v", res, err)
	}
	e = instances(readSettings(t, path))["claude-r_aaaaaa01"]
	if e["displayName"] != "Reviewer v2" || e["config"].(map[string]any)["homePath"] != "/h/c2" {
		t.Fatalf("not updated: %v", e)
	}
	if e["accentColor"] != "#fff" || e["enabled"] != false {
		t.Errorf("T3's own fields were dropped: %v", e)
	}
	if e["environment"] != nil {
		t.Errorf("an instance carries an environment: %v", e["environment"])
	}
	// nothing changed: nothing written
	fi1, _ := os.Stat(path)
	res, err = SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer v2", "/h/c2", ""}})
	fi2, _ := os.Stat(path)
	if err != nil || res.Changed() || !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatalf("an unchanged sync wrote: %+v %v", res, err)
	}
}

func TestSyncInstancesRefusesSettingsItCannotRead(t *testing.T) {
	for name, body := range map[string]string{"not json": `{"providerInstances":`, "instances not an object": `{"providerInstances":[1]}`} {
		o, path := instFixture(t, body)
		if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R", "/h/c", ""}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Errorf("%s: the file was changed: %s", name, b)
		}
	}
	o, _ := instFixture(t, "")
	if _, err := SyncInstances(o, []RoleInstance{{"bad id", "R", "/h/c", ""}}); err == nil {
		t.Error("a bad role id was accepted")
	}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R", "relative", ""}}); err == nil {
		t.Error("a relative config directory was accepted")
	}
}

// A copy of Home's entry under another key is the user's: a sync neither changes nor removes it, nor the shim.
func TestSyncInstancesLeavesACopyOfItsEntryUnderAnotherKey(t *testing.T) {
	o, path := instFixture(t, "")
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	copyOf := instances(m)["claude-r_aaaaaa01"]
	m["providerInstances"].(map[string]any)["my-copy"] = copyOf
	b, _ := json.Marshal(m)
	os.WriteFile(path, b, 0o600)
	// the role goes away: Home's entry and shim go, the user's copy stays, and the shim file is not touched through it
	res, err := SyncInstances(o, nil)
	if err != nil || !reflect.DeepEqual(res.Removed, []string{"claude-r_aaaaaa01"}) {
		t.Fatalf("%+v %v", res, err)
	}
	in := instances(readSettings(t, path))
	if len(in) != 1 || in["my-copy"] == nil {
		t.Fatalf("instances: %v", in)
	}
	// the same with the role still there: the copy is not Home's, and is not updated
	o2, path2 := instFixture(t, "")
	SyncInstances(o2, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}})
	m = readSettings(t, path2)
	cp := instances(m)["claude-r_aaaaaa01"]
	cp["displayName"] = "My own"
	m["providerInstances"].(map[string]any)["my-copy"] = cp
	b, _ = json.Marshal(m)
	os.WriteFile(path2, b, 0o600)
	if _, err := SyncInstances(o2, []RoleInstance{{"r_aaaaaa01", "Renamed", "/h/c", ""}}); err != nil {
		t.Fatal(err)
	}
	in = instances(readSettings(t, path2))
	if in["my-copy"]["displayName"] != "My own" || in["claude-r_aaaaaa01"]["displayName"] != "Renamed" {
		t.Fatalf("instances: %v", in)
	}
}

// A key that is a path can't make a removal reach outside the shim folder, and an entry whose key isn't its shim's
// name is not Home's.
func TestSyncInstancesRemovesNothingOutsideTheShimFolder(t *testing.T) {
	o, path := instFixture(t, "")
	shims := filepath.Join(o.Dir, "t3", "bin")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(o.Dir, "y")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(shims, "claude-inner")
	os.WriteFile(inner, []byte("keep"), 0o755)
	hostile := map[string]any{
		"claude-x/../../y": map[string]any{"driver": "claudeAgent", "config": map[string]any{"binaryPath": filepath.Join(shims, "claude-x/../../y")}},
		"y":                map[string]any{"driver": "claudeAgent", "config": map[string]any{"binaryPath": filepath.Join(shims, "claude-inner")}},
		"claude-other":     map[string]any{"driver": "claudeAgent", "config": map[string]any{"binaryPath": filepath.Join(shims, "claude-inner")}},
		"claude-up":        map[string]any{"driver": "claudeAgent", "config": map[string]any{"binaryPath": filepath.Join(o.Dir, "claude-up")}},
	}
	b, _ := json.Marshal(map[string]any{"providerInstances": hostile})
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, b, 0o600)
	res, err := SyncInstances(o, nil)
	if err != nil || res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	for _, f := range []string{victim, inner} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed", f)
		}
	}
	if got := instances(readSettings(t, path)); len(got) != 4 {
		t.Errorf("instances: %v", got)
	}
}

// T3 (or the user) writes the file while the sync works: the sync starts again from what it holds, so nothing of
// theirs is lost.
func TestSyncInstancesDoesNotOverwriteAConcurrentChange(t *testing.T) {
	o, path := instFixture(t, `{"theme":"dark"}`)
	n := 0
	beforeRename = func() {
		if n++; n == 1 {
			os.WriteFile(path, []byte(`{"theme":"light","providerInstances":{"codex":{"driver":"codex"}}}`), 0o600)
		}
	}
	defer func() { beforeRename = nil }()
	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}})
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	m := readSettings(t, path)
	in := instances(m)
	if m["theme"] != "light" || in["codex"] == nil || in["claude-r_aaaaaa01"] == nil {
		t.Fatalf("the concurrent change was lost: %v", m)
	}
	if n != 2 {
		t.Errorf("the file was written %d times", n)
	}
}

func TestSyncInstancesGivesUpWhenTheFileNeverSettles(t *testing.T) {
	o, path := instFixture(t, `{}`)
	i := 0
	beforeRename = func() { i++; os.WriteFile(path, []byte(`{"n":`+string(rune('0'+i%10))+`}`), 0o600) }
	defer func() { beforeRename = nil }()
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}}); err == nil {
		t.Fatal("no error for a file that keeps changing")
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Errorf("temporary files left: %v", ents)
	}
}

// A settings file that is a link is written where it really is, and stays a link.
func TestSyncInstancesWritesThroughASymlinkedSettingsFile(t *testing.T) {
	o, path := instFixture(t, "")
	real := filepath.Join(t.TempDir(), "mine.json")
	if err := os.WriteFile(real, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c", ""}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced by a file: %v %v", fi, err)
	}
	b, _ := os.ReadFile(real)
	if !strings.Contains(string(b), "claude-r_aaaaaa01") || !strings.Contains(string(b), `"theme": "dark"`) {
		t.Fatalf("the real file: %s", b)
	}
}

func projectInstances(t *testing.T, m map[string]any, project string) map[string]any {
	t.Helper()
	ovs, _ := m["projectSettingsOverrides"].(map[string]any)
	ov, _ := ovs[project].(map[string]any)
	pis, _ := ov["providerInstances"].(map[string]any)
	return pis
}

func enabledIn(pis map[string]any, id string) (bool, bool) {
	e, ok := pis[id].(map[string]any)
	if !ok {
		return false, false
	}
	v, ok := e["enabled"].(bool)
	return v, ok
}

// With T3's projects known, a role's instance is off globally and on only in the project of its own folder (followed
// through symlinks); a role pinned to no folder is on nowhere.
func TestSyncInstancesTurnsARoleOnOnlyInItsProject(t *testing.T) {
	o, path := instFixture(t, "")
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "b")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(d, "a-link")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	o.Projects = []T3Project{{ID: "pa", Root: link}, {ID: "pb", Root: b}}
	want := []RoleInstance{{"r_aaaaaa01", "Reviewer", "/h/c1", a}, {"r_aaaaaa02", "Builder", "/h/c2", b}, {"r_aaaaaa03", "Loose", "/h/c3", ""}}
	res, err := SyncInstances(o, want)
	if err != nil || !res.Projects || !res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	m := readSettings(t, path)
	for id, e := range instances(m) {
		if e["enabled"] != false {
			t.Errorf("%s is on globally: %v", id, e["enabled"])
		}
	}
	for _, c := range []struct {
		project, id string
		on          bool
	}{
		{"pa", "claude-r_aaaaaa01", true}, {"pa", "claude-r_aaaaaa02", false}, {"pa", "claude-r_aaaaaa03", false},
		{"pb", "claude-r_aaaaaa01", false}, {"pb", "claude-r_aaaaaa02", true}, {"pb", "claude-r_aaaaaa03", false},
	} {
		if on, ok := enabledIn(projectInstances(t, m, c.project), c.id); !ok || on != c.on {
			t.Errorf("%s in %s: on=%v set=%v, want on=%v", c.id, c.project, on, ok, c.on)
		}
	}
	// an instance switched on globally (in T3) is off again at the next sync: only its project turns it on
	m["providerInstances"].(map[string]any)["claude-r_aaaaaa02"].(map[string]any)["enabled"] = true
	if bs, _ := json.Marshal(m); os.WriteFile(path, bs, 0o600) != nil {
		t.Fatal("write")
	}
	if res, err := SyncInstances(o, want); err != nil || !reflect.DeepEqual(res.Updated, []string{"claude-r_aaaaaa02"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if e := instances(readSettings(t, path))["claude-r_aaaaaa02"]; e["enabled"] != false {
		t.Errorf("an instance switched on globally stayed on: %v", e["enabled"])
	}
	// the same again: nothing to write
	fi1, _ := os.Stat(path)
	res, err = SyncInstances(o, want)
	fi2, _ := os.Stat(path)
	if err != nil || res.Changed() || !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatalf("an unchanged sync wrote: %+v %v", res, err)
	}
}

// The project sync changes only the role instances' switches: other providers' settings and other project settings
// stay, and a removed role's switch goes from every project.
func TestSyncInstancesProjectSettingsKeepWhatTheyDidNotWrite(t *testing.T) {
	o, path := instFixture(t, "")
	root := t.TempDir()
	o.Projects = []T3Project{{ID: "p1", Root: root}}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}, {"r_aaaaaa02", "R2", "/h/c2", root}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	ov := m["projectSettingsOverrides"].(map[string]any)["p1"].(map[string]any)
	ov["theme"] = "light"
	pis := ov["providerInstances"].(map[string]any)
	pis["cursor"] = map[string]any{"enabled": true}
	pis["codex"] = map[string]any{"enabled": false, "note": "mine"}
	m["projectSettingsOverrides"].(map[string]any)["elsewhere"] = map[string]any{"x": 1}
	bs, _ := json.Marshal(m)
	if err := os.WriteFile(path, bs, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}})
	if err != nil || !reflect.DeepEqual(res.Removed, []string{"claude-r_aaaaaa02"}) {
		t.Fatalf("%+v %v", res, err)
	}
	m = readSettings(t, path)
	p1 := projectInstances(t, m, "p1")
	if _, ok := p1["claude-r_aaaaaa02"]; ok {
		t.Error("a removed role's switch stayed")
	}
	if on, _ := enabledIn(p1, "claude-r_aaaaaa01"); !on {
		t.Error("the role's own switch went")
	}
	if !reflect.DeepEqual(p1["cursor"], map[string]any{"enabled": true}) || !reflect.DeepEqual(p1["codex"], map[string]any{"enabled": false, "note": "mine"}) {
		t.Errorf("other providers' settings changed: %v", p1)
	}
	ovs := m["projectSettingsOverrides"].(map[string]any)
	if ovs["p1"].(map[string]any)["theme"] != "light" || !reflect.DeepEqual(ovs["elsewhere"], map[string]any{"x": float64(1)}) {
		t.Errorf("other project settings changed: %v", ovs)
	}
}

// Without T3's projects (T3 not paired or not answering) no project setting is written; a new instance is off (its
// project turns it on at the next sync with the projects) and an existing one keeps its switch.
func TestSyncInstancesWithoutProjectsWritesNoProjectSettings(t *testing.T) {
	o, path := instFixture(t, `{"projectSettingsOverrides":{"p1":{"providerInstances":{"claude-r_aaaaaa01":{"enabled":true}}}}}`)
	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R", "/h/c", "/somewhere"}})
	if err != nil || res.Projects {
		t.Fatalf("%+v %v", res, err)
	}
	m := readSettings(t, path)
	if instances(m)["claude-r_aaaaaa01"]["enabled"] != false {
		t.Error("a new instance is on with no projects known")
	}
	if on, ok := enabledIn(projectInstances(t, m, "p1"), "claude-r_aaaaaa01"); !ok || !on {
		t.Error("a project setting changed with no projects known")
	}
	m["providerInstances"].(map[string]any)["claude-r_aaaaaa01"].(map[string]any)["enabled"] = true
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R", "/h/c", "/somewhere"}}); err != nil || res.Changed() {
		t.Fatalf("%+v %v", res, err)
	}
	if instances(readSettings(t, path))["claude-r_aaaaaa01"]["enabled"] != true {
		t.Error("an existing instance's switch changed with no projects known")
	}
}

// A role removed while T3's projects weren't known loses its switches at the next sync that knows them, in every
// project (also one T3 no longer lists); a user's own instance whose key looks like a role's keeps them.
func TestSyncInstancesDropsTheSwitchesOfAGoneRoleLater(t *testing.T) {
	root := t.TempDir()
	o, path := instFixture(t, "")
	o.Projects = []T3Project{{ID: "p1", Root: root}}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}, {"r_aaaaaa02", "R2", "/h/c2", root}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	m["projectSettingsOverrides"].(map[string]any)["unlisted"] = map[string]any{"providerInstances": map[string]any{"claude-r_aaaaaa02": map[string]any{"enabled": true}}}
	m["providerInstances"].(map[string]any)["claude-r_aaaaaa09"] = map[string]any{"driver": "claudeAgent", "config": map[string]any{"binaryPath": "/usr/local/bin/claude"}}
	m["projectSettingsOverrides"].(map[string]any)["p1"].(map[string]any)["providerInstances"].(map[string]any)["claude-r_aaaaaa09"] = map[string]any{"enabled": true}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	o.Projects = nil
	if res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}}); err != nil || len(res.Removed) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if _, ok := projectInstances(t, readSettings(t, path), "p1")["claude-r_aaaaaa02"]; !ok {
		t.Fatal("a project setting changed with no projects known")
	}
	o.Projects = []T3Project{{ID: "p1", Root: root}}
	res, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}})
	if err != nil || !res.Projects {
		t.Fatalf("%+v %v", res, err)
	}
	m = readSettings(t, path)
	for _, pid := range []string{"p1", "unlisted"} {
		if _, ok := projectInstances(t, m, pid)["claude-r_aaaaaa02"]; ok {
			t.Errorf("the gone role's switch stayed in %s", pid)
		}
	}
	if on, ok := enabledIn(projectInstances(t, m, "p1"), "claude-r_aaaaaa09"); !ok || !on {
		t.Error("the user's own instance lost its switch")
	}
}

// Project settings it can't read stop the sync before any shim is written or removed.
func TestSyncInstancesTouchesNoShimWhenProjectSettingsAreUnreadable(t *testing.T) {
	root := t.TempDir()
	o, path := instFixture(t, "")
	o.Projects = []T3Project{{ID: "p1", Root: root}}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	m["projectSettingsOverrides"].(map[string]any)["p2"] = map[string]any{"providerInstances": "x"}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa02", "R2", "/h/c2", root}}); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Stat(o.ShimPath("r_aaaaaa01")); err != nil {
		t.Error("the removed role's shim went although nothing was written")
	}
	if _, err := os.Stat(o.ShimPath("r_aaaaaa02")); err == nil {
		t.Error("a new role's shim was written although nothing was written")
	}
}

// Only the projects whose switches changed are written: settings of a project T3 didn't list stay as they are, even
// an empty one; a project listed twice is one project.
func TestSyncInstancesWritesOnlyTheProjectsItChanged(t *testing.T) {
	root := t.TempDir()
	o, path := instFixture(t, `{"projectSettingsOverrides":{"p3":{"providerInstances":null}}}`)
	o.Projects = []T3Project{{ID: "p1", Root: root}, {ID: "p1", Root: root}}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", root}}); err != nil {
		t.Fatal(err)
	}
	m := readSettings(t, path)
	ovs := m["projectSettingsOverrides"].(map[string]any)
	if !reflect.DeepEqual(ovs["p3"], map[string]any{"providerInstances": nil}) {
		t.Errorf("an unchanged project was rewritten: %v", ovs["p3"])
	}
	if on, ok := enabledIn(projectInstances(t, m, "p1"), "claude-r_aaaaaa01"); !ok || !on {
		t.Errorf("the role's project: %v", ovs["p1"])
	}
}

// A project's folder matches the role's by path even when it can't be resolved (gone, or not readable), as Home's own
// check does when it finds the role's project.
func TestSyncInstancesMatchesAnUnresolvableFolderByPath(t *testing.T) {
	o, path := instFixture(t, "")
	gone := filepath.Join(t.TempDir(), "gone", "proj")
	o.Projects = []T3Project{{ID: "p1", Root: gone + "/"}}
	if _, err := SyncInstances(o, []RoleInstance{{"r_aaaaaa01", "R1", "/h/c1", gone}}); err != nil {
		t.Fatal(err)
	}
	if on, ok := enabledIn(projectInstances(t, readSettings(t, path), "p1"), "claude-r_aaaaaa01"); !ok || !on {
		t.Error("the role is off in the project of its own folder")
	}
}
