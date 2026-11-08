package home

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentbridge"
	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

type projectT3 struct {
	process   *agentbridge.Process
	client    *t3.Client
	project   string
	binding   agentsandbox.Binding
	policyKey [32]byte
}

const projectIdleTimeout = 5 * time.Minute
const projectPort = 37173

func (t *T3Home) readyForThread(ctx context.Context, th t3Thread) (*t3.Client, error) {
	if th.Provider == "cursor" && th.Environment == "" {
		return nil, errors.New("project T3: missing persisted environment binding")
	}
	return t.readyForTaskProvider(ctx, th.Project, th.Role, th.Provider, th.Environment, th.Task)
}
func (t *T3Home) readyForProvider(ctx context.Context, project, role, provider, environment string) (*t3.Client, error) {
	return t.readyForTaskProvider(ctx, project, role, provider, environment, "")
}
func (t *T3Home) readyForTaskProvider(ctx context.Context, project, role, provider, environment, task string) (*t3.Client, error) {
	if task != "" {
		r, err := t.h.Agent.T3Role(role)
		if err != nil {
			return nil, err
		}
		r.Release()
		if project != r.Project {
			return nil, errors.New("task: role project changed")
		}
		if _, err := agenttask.Resolve(t.h.Dir, task, project, role, roles.DirHash(r.ProjectDir)); err != nil {
			return nil, err
		}
	}

	if provider != "cursor" {
		if provider != "" {
			if _, ok := agentsandbox.Route(provider); !ok {
				return nil, errors.New("project T3: unsupported provider")
			}
		}
		return t.ready()
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("project T3: secure project environments require Linux")
	}
	if t.h.Agent == nil {
		return nil, errors.New("project T3: role manager unavailable")
	}
	r, err := t.h.Agent.T3Role(role)
	if err != nil {
		return nil, err
	}
	defer r.Release()
	if project != "" && project != r.Project {
		return nil, errors.New("project T3: role was reassigned")
	}
	sourceHash := roles.DirHash(r.ProjectDir)
	workspace := r.ProjectDir
	if task != "" {
		w, err := agenttask.Resolve(t.h.Dir, task, r.Project, r.ID, sourceHash)
		if err != nil {
			return nil, err
		}
		workspace = w.Dir
	}
	hash := roles.DirHash(workspace)
	if environment != "" && environment != hash {
		return nil, errors.New("project T3: project folder changed; refusing to relocate a thread")
	}
	binding := agentsandbox.Binding{HomeDir: t.h.Dir, Role: r.ID, Project: r.Project, DirHash: sourceHash, Provider: "cursor", Task: task}
	// Re-resolve config and committed role even when a cached server is alive.
	l, err := ResolveProjectServer(binding, filepath.Join(t.h.Dir, "config.json"))
	if err != nil {
		return nil, err
	}
	pin, err := t.pin()
	if err != nil {
		return nil, err
	}
	key := projectPolicyKey(l, pin.Version)
	t.projectMu.Lock()
	defer t.projectMu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if t.projectServers == nil {
		life, stop := context.WithCancel(t.baseCtx())
		t.projectStop = stop
		t.projectDone = make(chan struct{})
		t.projectServers = map[string]*projectT3{}
		go t.reapProjectServers(life, t.projectDone)
	}
	if old := t.projectServers[hash]; old != nil {
		if old.process.Alive() && old.policyKey == key {
			if err := projectCursorInstance(t.projectState(hash), role, r.Name); err != nil {
				return nil, err
			}
			return old.client, nil
		}
		old.client.Close()
		old.process.Close()
		delete(t.projectServers, hash)
	}
	base := filepath.Dir(t.projectState(hash))
	state := t.projectState(hash)
	for _, d := range []string{base, state, filepath.Join(base, "settings"), filepath.Join(base, "hidden-home"), filepath.Join(base, "tmp"), filepath.Join(state, "home"), filepath.Join(state, "t3")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, errors.New("project T3: cannot create private state")
		}
		st, err := os.Lstat(d)
		if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
			return nil, errors.New("project T3: state directories must be private and not symlinks")
		}
	}
	if err := projectCursorInstance(state, role, r.Name); err != nil {
		return nil, err
	}
	relay := t.h.Fresh().AgentSandbox.AgentExec
	if relay == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		relay = filepath.Join(filepath.Dir(self), "rubi-agent-exec")
	}
	l.Policy.Home = filepath.Join(base, "hidden-home")
	l.Policy.Login = state
	l.Policy.Tmp = filepath.Join(base, "tmp")
	l.SettingsDir = filepath.Join(base, "settings")
	native := t.exePath(pin.Version)
	if st, err := os.Stat(native); err != nil || st.IsDir() {
		return nil, errors.New("project T3: pinned runtime unavailable; install T3 first")
	}
	l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, filepath.Dir(native), t.nodeBin(), relay)
	l.SearchPath = append(l.SearchPath, filepath.Dir(t.nodeBin()))
	if err := checkCredentialReadGrants(t.h.Fresh().AgentSandbox, l.Policy.RuntimeRead); err != nil {
		return nil, err
	}
	credentials, err := agentbridge.Credentials(l.Policy, l.CredentialsFile)
	if err != nil {
		return nil, err
	}
	if credentials["CURSOR_API_KEY"] == "" {
		return nil, errors.New("project T3: provision CURSOR_API_KEY via the provider's private credentials_file/project_credentials")
	}
	stateEnv, err := agentbridge.StateEnvironment(state)
	if err != nil {
		return nil, err
	}
	for key, value := range stateEnv {
		credentials[key] = value
	}
	command := []string{native, "serve", "--base-dir", filepath.Join(state, "t3"), "--port", strconv.Itoa(projectPort), "--host", "127.0.0.1", "--no-browser"}
	pair := []string{native, "auth", "pairing", "create", "--base-dir", filepath.Join(state, "t3"), "--ttl", "10m", "--label", "Rubi project", "--json"}
	for _, scope := range t3.PairingScopes(string(t.Ceiling)) {
		pair = append(pair, "--scope", scope)
	}
	process, ready, err := agentbridge.Start(t.baseCtx(), l, relay, agentbridge.Bootstrap{Command: command, PairCommand: pair, Environment: credentials, Port: projectPort})
	if err != nil {
		return nil, err
	}
	url := "http://127.0.0.1:" + strconv.Itoa(projectPort)
	tok, err := t3.Pair(ctx, &http.Client{Transport: process, Timeout: 30 * time.Second}, url, ready.Credential, string(t.Ceiling), "Rubi project")
	if err != nil {
		process.Close()
		return nil, errors.New("project T3: native pairing failed")
	}
	client := t3.NewWithTransport(url, tok, t.Ceiling, process)
	t.projectServers[hash] = &projectT3{process: process, client: client, project: r.Project, binding: binding, policyKey: key}
	return client, nil
}
func (t *T3Home) projectState(hash string) string {
	return filepath.Join(t.root(), "environments", hash, "state")
}

// The project can edit its state. Anchor all host-side operations to the stable
// state root: never follow an agent-created settings/userdata symlink into HOME.
func projectCursorInstance(state, role, name string) error {
	root, err := os.OpenRoot(state)
	if err != nil {
		return errors.New("project T3: state root unavailable")
	}
	defer root.Close()
	dir := filepath.Join("t3", "userdata")
	if err := root.MkdirAll(dir, 0700); err != nil {
		return errors.New("project T3: unsafe userdata path")
	}
	path := filepath.Join(dir, "settings.json")
	settings := map[string]json.RawMessage{}
	data, err := root.ReadFile(path)
	if err == nil {
		if json.Unmarshal(data, &settings) != nil || settings == nil {
			return errors.New("project T3: unreadable settings")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("project T3: cannot read settings safely")
	}
	instances := map[string]json.RawMessage{}
	if data := settings["providerInstances"]; len(data) > 0 {
		if json.Unmarshal(data, &instances) != nil || instances == nil {
			return errors.New("project T3: unreadable provider instances")
		}
	}
	entry, err := json.Marshal(map[string]any{"driver": "cursor", "displayName": name, "enabled": true, "config": map[string]any{}})
	if err != nil {
		return err
	}
	instances["rubi-cursor-"+role] = entry
	settings["providerInstances"], _ = json.Marshal(instances)
	data, err = json.Marshal(settings)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".rubi-settings-"+newMsgID())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("project T3: cannot stage settings")
	}
	defer root.Remove(tmp)
	if _, err = f.Write(data); err == nil {
		err = f.Close()
	} else {
		f.Close()
	}
	if err != nil {
		return errors.New("project T3: cannot write settings")
	}
	if err := root.Rename(tmp, path); err != nil {
		return errors.New("project T3: cannot commit settings")
	}
	return nil
}

func (t *T3Home) reapProjectServers(ctx context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			t.projectMu.Lock()
			for hash, s := range t.projectServers {
				if _, err := ResolveProjectServer(s.binding, filepath.Join(t.h.Dir, "config.json")); err != nil {
					s.process.Close()
					s.client.Close()
					delete(t.projectServers, hash)
					continue
				}
				busy := false
				for _, th := range t.threads.snapshot() {
					if th.Provider == "cursor" && th.Environment == hash && th.Running && !th.Closed {
						busy = true
						break
					}
				}
				if !busy && s.process.StopIdle(now, projectIdleTimeout) {
					s.client.Close()
					delete(t.projectServers, hash)
				}
			}
			t.projectMu.Unlock()
		}
	}
}
func (t *T3Home) stopProjectServers() {
	t.projectMu.Lock()
	stop, done, servers := t.projectStop, t.projectDone, t.projectServers
	t.projectStop = nil
	t.projectDone = nil
	t.projectServers = nil
	t.projectMu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	for _, s := range servers {
		s.process.Close()
		s.client.Close()
	}
}

// Role-specific working directories are replaced by the project server's own
// state. Everything affecting its grants/credentials/runtime invalidates reuse.
func projectPolicyKey(l agentsandbox.Launch, version string) [32]byte {
	p := l.Policy
	p.Home = ""
	p.Login = ""
	p.Tmp = ""
	data, _ := json.Marshal(struct {
		Policy                    agentsandbox.Policy
		SRT, Credentials, Version string
		Search                    []string
	}{p, l.SRT, l.CredentialsFile, version, l.SearchPath})
	return sha256.Sum256(data)
}

// Stopping must not fail because an agent damaged its own workspace. Use only
// the already-owned client; never restart a provider or widen a policy to stop it.
func (t *T3Home) readyForStop(ctx context.Context, th t3Thread) (*t3.Client, error) {
	if th.Task == "" {
		return t.readyForThread(ctx, th)
	}
	if th.Provider != "cursor" {
		return t.ready()
	}
	t.projectMu.Lock()
	defer t.projectMu.Unlock()
	server := t.projectServers[th.Environment]
	if server == nil || !server.process.Alive() || server.binding.Task != th.Task || server.binding.Role != th.Role || server.binding.Project != th.Project {
		return nil, errors.New("task: owned project server is not running")
	}
	return server.client, nil
}
