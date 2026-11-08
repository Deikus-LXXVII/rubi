package t3vm

// The role instances in T3's settings: for each role Home writes a provider instance `claude-<role>` whose binary is
// a shim that runs `rubi-home agent-vm`, so a thread started on that instance runs its claude in the role's VM.
// T3 watches its settings file; the instances Home didn't write are never touched.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// InstancePrefix starts the id of every instance Home writes.
const InstancePrefix = "claude-"

// RoleInstance is a role as an instance needs it.
type RoleInstance struct {
	ID, Name  string
	ClaudeDir string // the role's config directory: the instance's homePath
	// ProjectDir is the project the role is pinned to: its instance is turned on only in T3's project of that folder.
	ProjectDir string
}

// T3Project is one of T3's projects: its id and its workspace root.
type T3Project struct{ ID, Root string }

// InstanceOptions say where things are.
type InstanceOptions struct {
	// T3Base is the T3 server's base folder (settings are in T3Base/userdata/settings.json).
	T3Base string
	// Dir is Home's directory; the shims are in Dir/t3/bin.
	Dir string
	// RubiHome is the path of the rubi-home binary the shims run.
	RubiHome string
	// Projects are T3's projects, when known. Then each role's instance is on only in its own project (T3's
	// projectSettingsOverrides, which T3 checks when a thread is dispatched) and off everywhere else. Other providers'
	// settings are left as they are. Unknown (nil): no project setting is written or changed, and a new instance is off.
	// Known but empty: no project is the role's, so the instances are off.
	Projects []T3Project
}

// SyncResult says what a sync changed.
type SyncResult struct {
	Added, Updated, Removed []string
	// Skipped names instances that have a role's id but are not Home's: they are left as they are.
	Skipped []string
	// Projects says the per-project settings changed.
	Projects bool
}

// Changed says whether the settings file was written.
func (r SyncResult) Changed() bool {
	return len(r.Added)+len(r.Updated)+len(r.Removed) > 0 || r.Projects
}

func (o InstanceOptions) shimDir() string { return filepath.Join(o.Dir, "t3", "bin") }
func (o InstanceOptions) settingsPath() string {
	return filepath.Join(o.T3Base, "userdata", "settings.json")
}

// ShimPath is the shim of a role.
func (o InstanceOptions) ShimPath(roleID string) string {
	return filepath.Join(o.shimDir(), InstancePrefix+roleID)
}

// shQuote quotes s for /bin/sh: one single-quoted word.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ShimScript is the text of a role's shim. It names Home's directory, because T3 runs it with an environment of its
// own (Home starts T3 with almost none) and `rubi-home agent-vm` must find Home's socket.
func ShimScript(rubiHome, dir, roleID string) ([]byte, error) {
	for _, p := range []string{rubiHome, dir} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\n\r\x00") {
			return nil, errors.New("t3vm: a path can't go into a script")
		}
	}
	if !roles.IsID(roleID) {
		return nil, errors.New("t3vm: bad role id")
	}
	return []byte("#!/bin/sh\nRUBI_HOME_DIR=" + shQuote(dir) + "\nexport RUBI_HOME_DIR\nexec " + shQuote(rubiHome) + " agent-vm --role " + shQuote(roleID) + ` -- "$@"` + "\n"), nil
}

// SyncInstances makes T3's settings hold one instance per given role, and none of Home's for a role that is gone.
// Everything else in the file stays as it is (other settings, instances written by the user).
func SyncInstances(o InstanceOptions, want []RoleInstance) (SyncResult, error) {
	path := o.settingsPath()
	// the file may be a link (the user keeps their settings elsewhere): write where it really is
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	} else if !errors.Is(err, os.ErrNotExist) {
		return SyncResult{}, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		raw, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return SyncResult{}, err
		}
		out, res, err := o.syncOnce(want, raw)
		if err != nil || !res.Changed() {
			return res, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return res, err
		}
		done, err := writeIfUnchanged(path, raw, out)
		if err != nil {
			return res, err
		}
		if done {
			return res, nil
		}
		// T3 (or the user) wrote the file while this was computed: compute again from what it holds now
	}
	return SyncResult{}, errors.New("t3vm: T3's settings kept changing while the instances were written; try again")
}

// beforeRename is a test seam: it runs between the check of the file and the rename.
var beforeRename func()

// writeIfUnchanged replaces path with out when it still holds old (and doesn't exist when old is nil), through a
// temporary file that is synced first, and syncs the folder after. It reports false, having written nothing, when the
// file changed.
func writeIfUnchanged(path string, old, out []byte) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".settings-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if beforeRename != nil {
		beforeRename()
	}
	now, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if !bytes.Equal(now, old) {
		return false, nil
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return true, nil
}

// syncOnce computes the new settings text from the current one (and writes the shims).
func (o InstanceOptions) syncOnce(want []RoleInstance, raw []byte) ([]byte, SyncResult, error) {
	var res SyncResult
	top := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, res, fmt.Errorf("t3vm: T3's settings aren't valid JSON, left alone: %w", err)
		}
	}
	insts := map[string]json.RawMessage{}
	if b, ok := top["providerInstances"]; ok && string(b) != "null" {
		if err := json.Unmarshal(b, &insts); err != nil {
			return nil, res, fmt.Errorf("t3vm: T3's providerInstances aren't an object, left alone: %w", err)
		}
	}
	var po *projectOverrides
	if o.Projects != nil {
		var err error
		if po, err = readProjectOverrides(top); err != nil {
			return nil, res, err
		}
	}
	shims := o.shimDir()
	// ours: the entry is Home's when its binary is a shim in Home's folder and the key is that shim's name. A copy of
	// Home's entry under another key (the user duplicated it) is the user's.
	ours := func(key string, entry json.RawMessage) bool {
		var e struct {
			Config struct {
				BinaryPath string `json:"binaryPath"`
			} `json:"config"`
		}
		if json.Unmarshal(entry, &e) != nil {
			return false
		}
		bp := e.Config.BinaryPath
		return strings.HasPrefix(key, InstancePrefix) && filepath.Dir(bp) == shims && filepath.Base(bp) == key
	}
	wanted := map[string]bool{}
	sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
	for _, r := range want {
		script, err := ShimScript(o.RubiHome, o.Dir, r.ID)
		if err != nil {
			return nil, res, err
		}
		if !filepath.IsAbs(r.ClaudeDir) {
			return nil, res, fmt.Errorf("t3vm: role %s has no config directory", r.ID)
		}
		id := InstancePrefix + r.ID
		if old, exists := insts[id]; exists && !ours(id, old) {
			res.Skipped = append(res.Skipped, id)
			continue
		}
		wanted[id] = true
		if err := os.MkdirAll(shims, 0o755); err != nil {
			return nil, res, err
		}
		shim := o.ShimPath(r.ID)
		if err := putFile(shim, script, 0o755); err != nil {
			return nil, res, err
		}
		entry := map[string]json.RawMessage{}
		old, exists := insts[id]
		if exists {
			_ = json.Unmarshal(old, &entry) // T3's own additions to the entry stay
		}
		cfg := map[string]json.RawMessage{}
		if b, ok := entry["config"]; ok {
			_ = json.Unmarshal(b, &cfg)
		}
		cfg["binaryPath"], cfg["homePath"] = jstr(shim), jstr(r.ClaudeDir)
		entry["driver"], entry["displayName"], entry["config"] = jstr("claudeAgent"), jstr(r.Name), mustMarshal(cfg)
		// Off for every project but where a project setting turns it on. A new instance is off too while T3's projects
		// aren't known: a role is pinned to its folder, and the next sync with them turns it on there.
		if _, ok := entry["enabled"]; o.Projects != nil || !ok {
			entry["enabled"] = json.RawMessage("false")
		}
		delete(entry, "environment") // Home gives a session its environment; an instance carries none
		nb := mustMarshal(entry)
		if exists && jsonEqual(old, nb) {
			continue
		}
		insts[id] = nb
		if exists {
			res.Updated = append(res.Updated, id)
		} else {
			res.Added = append(res.Added, id)
		}
	}
	for id, entry := range insts {
		if !wanted[id] && ours(id, entry) {
			delete(insts, id)
			res.Removed = append(res.Removed, id)
			// by the shim's own name, which ours() has checked is the key and sits in the shim folder
			var e struct{ Config struct{ BinaryPath string } }
			_ = json.Unmarshal(entry, &e)
			_ = os.Remove(filepath.Join(shims, filepath.Base(e.Config.BinaryPath)))
		}
	}
	sort.Strings(res.Removed)
	if o.Projects != nil {
		res.Projects = o.syncProjects(top, po, want, wanted, insts)
	}
	if !res.Changed() {
		return nil, res, nil
	}
	top["providerInstances"] = mustMarshal(insts)
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, res, err
	}
	return append(out, '\n'), res, nil
}

func jstr(s string) json.RawMessage { return mustMarshal(s) }

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// jsonEqual compares two JSON texts as values.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && string(mustMarshal(x)) == string(mustMarshal(y))
}

// sameDir says whether a and b are one folder: the same path, or the same once their symlinks are followed (Home's
// check when it finds a role's T3 project, so a project it uses is the one the role is turned on in).
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, ea := filepath.EvalSymlinks(a)
	rb, eb := filepath.EvalSymlinks(b)
	return ea == nil && eb == nil && ra == rb
}

// projectOverrides is T3's per-project settings, each project's provider switches read out: the project sync changes
// only those. It refuses settings it can't read, before the sync writes or removes any shim.
type projectOverrides struct {
	ovs map[string]map[string]json.RawMessage
	pis map[string]map[string]json.RawMessage // project id → provider instance id → its setting
}

func readProjectOverrides(top map[string]json.RawMessage) (*projectOverrides, error) {
	po := &projectOverrides{ovs: map[string]map[string]json.RawMessage{}, pis: map[string]map[string]json.RawMessage{}}
	if b, ok := top["projectSettingsOverrides"]; ok && string(b) != "null" {
		if err := json.Unmarshal(b, &po.ovs); err != nil {
			return nil, fmt.Errorf("t3vm: T3's projectSettingsOverrides aren't an object, left alone: %w", err)
		}
	}
	for id, ov := range po.ovs {
		if ov == nil {
			continue
		}
		pis := map[string]json.RawMessage{}
		if b, ok := ov["providerInstances"]; ok && string(b) != "null" {
			if err := json.Unmarshal(b, &pis); err != nil {
				return nil, fmt.Errorf("t3vm: the provider settings of T3's project %s aren't an object, left alone: %w", id, err)
			}
		}
		po.pis[id] = pis
	}
	return po, nil
}

// syncProjects writes, in each of T3's projects, whether each role's instance is on: on only in the project of the
// role's own folder, off in every other (a choice made in T3 for another project doesn't stay: the role is pinned to its
// folder). The switch of a role instance that no longer exists goes from every project, also one T3 didn't list. Other
// project settings, other providers' among them, stay as they are. insts is the instances after the sync. It says
// whether anything changed.
func (o InstanceOptions) syncProjects(top map[string]json.RawMessage, po *projectOverrides, want []RoleInstance, wanted map[string]bool, insts map[string]json.RawMessage) bool {
	on := json.RawMessage(`{"enabled":true}`)
	off := json.RawMessage(`{"enabled":false}`)
	dirty := map[string]bool{}
	for _, p := range o.Projects {
		if p.ID == "" {
			continue
		}
		pis := po.pis[p.ID]
		if pis == nil {
			pis = map[string]json.RawMessage{}
		}
		for _, r := range want {
			id := InstancePrefix + r.ID
			if !wanted[id] {
				continue
			}
			v := off
			if r.ProjectDir != "" && sameDir(r.ProjectDir, p.Root) {
				v = on
			}
			if old, ok := pis[id]; !ok || !jsonEqual(old, v) {
				pis[id] = v
				dirty[p.ID] = true
			}
		}
		po.pis[p.ID] = pis
	}
	for pid, pis := range po.pis {
		for id := range pis {
			// a role's switch (the key is a role instance's) whose instance is gone; a key with an instance that isn't
			// Home's is the user's, and stays
			if _, exists := insts[id]; !exists && !wanted[id] && strings.HasPrefix(id, InstancePrefix) && roles.IsID(strings.TrimPrefix(id, InstancePrefix)) {
				delete(pis, id)
				dirty[pid] = true
			}
		}
	}
	if len(dirty) == 0 {
		return false
	}
	for pid := range dirty {
		ov := po.ovs[pid]
		if ov == nil {
			ov = map[string]json.RawMessage{}
		}
		ov["providerInstances"] = mustMarshal(po.pis[pid])
		po.ovs[pid] = ov
	}
	top["projectSettingsOverrides"] = mustMarshal(po.ovs)
	return true
}
