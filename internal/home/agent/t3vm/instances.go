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
}

// InstanceOptions say where things are.
type InstanceOptions struct {
	// T3Base is the T3 server's base folder (settings are in T3Base/userdata/settings.json).
	T3Base string
	// Dir is Home's directory; the shims are in Dir/t3/bin.
	Dir string
	// RubiHome is the path of the rubi-home binary the shims run.
	RubiHome string
}

// SyncResult says what a sync changed.
type SyncResult struct {
	Added, Updated, Removed []string
	// Skipped names instances that have a role's id but are not Home's: they are left as they are.
	Skipped []string
}

// Changed says whether the settings file was written.
func (r SyncResult) Changed() bool { return len(r.Added)+len(r.Updated)+len(r.Removed) > 0 }

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
		if _, ok := entry["enabled"]; !ok {
			entry["enabled"] = json.RawMessage("true")
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
