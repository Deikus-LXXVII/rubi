package t3instances

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// SandboxRole is resolved by Rubi's manager, not from T3's workspace or a prompt.
type SandboxRole struct{ ID, Name, Project, ProjectDir, ClaudeDir, Task, Workspace string }

func sandboxInstanceID(role, driver string) string {
	if driver == "claudeAgent" {
		return InstancePrefix + role
	}
	return "rubi-" + strings.ToLower(driver) + "-" + role
}

// SyncSandboxInstances writes fixed per-role/provider shims and project switches.
// Personal instances are untouched. Cursor uses its separate project server, not a shim.
func SyncSandboxInstances(o InstanceOptions, agentExec string, want []SandboxRole) (SyncResult, error) {
	path := o.settingsPath()
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
		out, res, err := o.sandboxOnce(agentExec, want, raw)
		if err != nil || !res.Changed() {
			return res, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return res, err
		}
		done, err := writeIfUnchanged(path, raw, out)
		if err != nil || done {
			return res, err
		}
	}
	return SyncResult{}, errors.New("sandbox: T3 settings kept changing; no session may start")
}

func (o InstanceOptions) sandboxOnce(agentExec string, want []SandboxRole, raw []byte) ([]byte, SyncResult, error) {
	var res SyncResult
	top := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, res, errors.New("sandbox: invalid T3 settings")
		}
	}
	insts := map[string]json.RawMessage{}
	if v := top["providerInstances"]; len(v) > 0 {
		if err := json.Unmarshal(v, &insts); err != nil {
			return nil, res, err
		}
	}
	po, err := readProjectOverrides(top)
	if err != nil {
		return nil, res, err
	}
	if !filepath.IsAbs(agentExec) || strings.ContainsAny(agentExec, "\r\n\x00") {
		return nil, res, errors.New("sandbox: absolute rubi-agent-exec installation path required")
	}
	if top == nil {
		return nil, res, errors.New("sandbox: T3 settings must be an object")
	}
	if insts == nil {
		insts = map[string]json.RawMessage{}
	}
	if po.ovs == nil {
		po.ovs = map[string]map[string]json.RawMessage{}
	}
	res.ProjectsKnown = o.Projects != nil
	// Track ownership by exact managed pathname, including pre-srt Claude shims.
	ours := func(id string, e json.RawMessage) bool {
		var entry struct{ Config map[string]json.RawMessage }
		if json.Unmarshal(e, &entry) != nil {
			return false
		}
		var bp string
		for _, key := range []string{"binaryPath", "commandPath"} {
			if v := entry.Config[key]; len(v) > 0 {
				_ = json.Unmarshal(v, &bp)
				if bp == filepath.Join(o.shimDir(), id) && (strings.HasPrefix(id, "claude-r_") || strings.HasPrefix(id, "rubi-")) {
					return true
				}
			}
		}
		return false
	}
	wanted := map[string]bool{}
	for _, r := range want {
		if !roles.IsID(r.ID) {
			return nil, res, errors.New("sandbox: bad role id")
		}
		for _, route := range agentsandbox.ProviderRoutes {
			if route.BinaryField == "" {
				continue
			}
			id := sandboxInstanceID(r.ID, route.Driver)
			if r.Task != "" {
				if !agenttask.ValidID(r.Task) {
					return nil, res, errors.New("sandbox: invalid task binding")
				}
				id += "-" + r.Task
			}
			old, exists := insts[id]
			if exists && !ours(id, old) {
				res.Skipped = append(res.Skipped, id)
				return nil, res, fmt.Errorf("sandbox: managed instance %s is occupied by a foreign instance", id)
			}
			wanted[id] = true
			base := filepath.Join(o.Dir, "t3", "sandbox", r.ID, route.Driver)
			login := filepath.Join(base, "login")
			if route.Driver == "claudeAgent" {
				login = r.ClaudeDir
			}
			if r.Task != "" {
				base = filepath.Join(base, "tasks", r.Task)
			}
			for _, dir := range []string{o.shimDir(), base, filepath.Join(base, "settings"), filepath.Join(base, "home"), filepath.Join(base, "tmp"), login} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					return nil, res, err
				}
				info, err := os.Lstat(dir)
				if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
					return nil, res, errors.New("sandbox: runtime directories must be private and not symlinks")
				}
			}
			binding := agentsandbox.Binding{HomeDir: o.Dir, Role: r.ID, Project: r.Project, DirHash: roles.DirHash(r.ProjectDir), Provider: route.Driver, Task: r.Task}
			// An unresolved role remains disabled; the resolver also refuses the empty project.
			if r.ProjectDir == "" {
				binding.DirHash = ""
			}
			config := filepath.Join(base, "binding.json")
			if err := putFile(config, mustMarshal(binding), 0600); err != nil {
				return nil, res, err
			}
			script, err := SandboxShimScript(agentExec, config)
			if err != nil {
				return nil, res, err
			}
			shim := filepath.Join(o.shimDir(), id)
			if err := putFile(shim, script, 0700); err != nil {
				return nil, res, err
			}
			field := route.BinaryField
			entryFields := map[string]json.RawMessage{}
			if exists {
				_ = json.Unmarshal(old, &entryFields)
			}
			cfg := map[string]any{}
			if oldConfig := entryFields["config"]; len(oldConfig) > 0 {
				if err := json.Unmarshal(oldConfig, &cfg); err != nil {
					return nil, res, errors.New("sandbox: invalid managed provider config")
				}
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			cfg[field] = shim
			switch route.Driver {
			case "claudeAgent":
				cfg["homePath"] = login
			case "codex":
				cfg["homePath"] = login
				cfg["setupMode"] = "existing"
			case "opencode":
				// A remote URL bypasses binaryPath entirely. Managed instances
				// always own a local confined server and a strong bridge secret.
				cfg["serverUrl"] = ""
				password, _ := cfg["serverPassword"].(string)
				if len(password) < 32 {
					secret := make([]byte, 32)
					if _, err := rand.Read(secret); err != nil {
						return nil, res, errors.New("sandbox: cannot provision OpenCode bridge authentication")
					}
					cfg["serverPassword"] = base64.RawURLEncoding.EncodeToString(secret)
				}
			case "acpRegistry":
				cfg["source"] = "local"
				if _, ok := cfg["commandArgs"]; !ok {
					cfg["commandArgs"] = []string{}
				}
			}
			name := r.Name + " · " + route.Driver
			entryFields["driver"], entryFields["displayName"], entryFields["enabled"], entryFields["config"] = jstr(route.Driver), jstr(name), json.RawMessage("false"), mustMarshal(cfg)
			delete(entryFields, "environment")
			entry := mustMarshal(entryFields)
			if !exists {
				res.Added = append(res.Added, id)
			} else if !jsonEqual(old, entry) {
				res.Updated = append(res.Updated, id)
			}
			insts[id] = entry
			// Reset stale overrides too: an old project must never retain access after a rebind.
			for _, p := range o.Projects {
				if po.pis[p.ID] == nil {
					po.pis[p.ID] = map[string]json.RawMessage{}
				}
			}
			for pid, pis := range po.pis {
				enabled := false
				for _, p := range o.Projects {
					root := r.ProjectDir
					if r.Task != "" {
						root = r.Workspace
					}
					if p.ID == pid && root != "" && sameDir(root, p.Root) && route.BinaryField != "" {
						enabled = true
					}
				}
				v := mustMarshal(map[string]bool{"enabled": enabled})
				if !jsonEqual(pis[id], v) {
					pis[id] = v
					res.Projects = true
				}
			}
		}
	}
	for id, entry := range insts {
		if !wanted[id] && ours(id, entry) {
			delete(insts, id)
			res.Removed = append(res.Removed, id)
			for _, pis := range po.pis {
				if _, ok := pis[id]; ok {
					delete(pis, id)
					res.Projects = true
				}
			}
		}
	}
	// A role may have disappeared while its instance was already absent from T3.
	for _, pis := range po.pis {
		for id := range pis {
			if _, exists := insts[id]; exists {
				continue
			}
			for _, route := range agentsandbox.ProviderRoutes {
				prefix := "rubi-" + strings.ToLower(route.Driver) + "-"
				if route.Driver == "claudeAgent" {
					prefix = InstancePrefix
				}
				if strings.HasPrefix(id, prefix) && roles.IsID(strings.TrimPrefix(id, prefix)) {
					delete(pis, id)
					res.Projects = true
					break
				}
			}
		}
	}
	if !res.Changed() {
		return nil, res, nil
	}
	for pid, pis := range po.pis {
		ov := po.ovs[pid]
		if ov == nil {
			ov = map[string]json.RawMessage{}
		}
		ov["providerInstances"] = mustMarshal(pis)
		po.ovs[pid] = ov
	}
	top["providerInstances"] = mustMarshal(insts)
	top["projectSettingsOverrides"] = mustMarshal(po.ovs)
	out, err := json.MarshalIndent(top, "", "  ")
	return append(out, '\n'), res, err
}

// SandboxShimScript puts a non-overridable config boundary before all provider arguments.
func SandboxShimScript(agentExec, config string) ([]byte, error) {
	for _, p := range []string{agentExec, config} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\r\n\x00") {
			return nil, errors.New("sandbox: invalid shim path")
		}
	}
	return []byte("#!/bin/sh\nexec " + shQuote(agentExec) + " --config " + shQuote(config) + ` -- "$@"` + "\n"), nil
}
