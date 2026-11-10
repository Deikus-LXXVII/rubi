package t3instances

// The role instances in T3's settings: for each role Home writes a provider instance `claude-<role>` whose binary is
// a trusted shim for rubi-agent-exec and the project-bound srt policy.
// T3 watches its settings file; the instances Home didn't write are never touched.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstancePrefix starts the id of every instance Home writes.
const InstancePrefix = "claude-"

// T3Project is one of T3's projects: its id and its workspace root.
type T3Project struct{ ID, Root string }

// InstanceOptions say where things are.
type InstanceOptions struct {
	// T3Base is the T3 server's base folder (settings are in T3Base/userdata/settings.json).
	T3Base string
	// Dir is Home's directory; the shims are in Dir/t3/bin.
	Dir string
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
	// ProjectsKnown says T3's projects were known, so each role's switch in them was written (or was right already).
	ProjectsKnown bool
}

// Changed says whether the settings file was written.
func (r SyncResult) Changed() bool {
	return len(r.Added)+len(r.Updated)+len(r.Removed) > 0 || r.Projects
}

func (o InstanceOptions) shimDir() string { return filepath.Join(o.Dir, "t3", "bin") }
func (o InstanceOptions) settingsPath() string {
	return filepath.Join(o.T3Base, "userdata", "settings.json")
}

// shQuote quotes s for /bin/sh: one single-quoted word.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var beforeRename func()

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
			return nil, fmt.Errorf("t3instances: T3's projectSettingsOverrides aren't an object, left alone: %w", err)
		}
	}
	for id, ov := range po.ovs {
		if ov == nil {
			continue
		}
		pis := map[string]json.RawMessage{}
		if b, ok := ov["providerInstances"]; ok && string(b) != "null" {
			if err := json.Unmarshal(b, &pis); err != nil {
				return nil, fmt.Errorf("t3instances: the provider settings of T3's project %s aren't an object, left alone: %w", id, err)
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

// putFile replaces trusted generated settings/shims atomically.
func putFile(path string, body []byte, perm os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, body) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() == perm {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".put-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
