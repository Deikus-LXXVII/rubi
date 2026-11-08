package home

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

func TestProjectServerUsesTrustedResolution(t *testing.T) {
	b, c, path, _ := sandboxFixture(t)
	a := c.AgentProjects[0]
	b.Provider = "cursor"
	c.AgentSandbox.Providers["cursor"] = agentsandbox.Provider{}
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	launch, err := ResolveProjectServer(b, path)
	if err != nil || len(launch.Policy.Projects) != 1 || launch.Policy.Projects[0] != a {
		t.Fatal("Cursor project was not resolved from Rubi config", err)
	}
	if _, err := ResolveAgentLaunch(b, path); err == nil {
		t.Fatal("Cursor SDK was enabled via CLI shim")
	}
	c.AgentProjects = c.AgentProjects[1:]
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	if _, err := ResolveProjectServer(b, path); err == nil {
		t.Fatal("removed project accepted")
	}
	b.Provider = "opencode"
	if _, err := ResolveProjectServer(b, path); err == nil {
		t.Fatal("wrong project server driver accepted")
	}
}
func TestProjectSettingsAreNativeAndPreservePersonalEntries(t *testing.T) {
	root := t.TempDir()
	userdata := filepath.Join(root, "t3", "userdata")
	os.MkdirAll(userdata, 0700)
	path := filepath.Join(userdata, "settings.json")
	os.WriteFile(path, []byte(`{"other":true,"providerInstances":{"personal":{"driver":"codex","config":{}}}}`), 0600)
	if err := projectCursorInstance(root, "r_aaaaaa01", "reviewer"); err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Other             bool
		ProviderInstances map[string]struct {
			Driver  string
			Enabled bool
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &settings) != nil {
		t.Fatal("native settings unreadable", err)
	}
	if !settings.Other || settings.ProviderInstances["personal"].Driver != "codex" || !settings.ProviderInstances["rubi-cursor-r_aaaaaa01"].Enabled {
		t.Fatal("provider entries lost or Cursor not enabled")
	}
	if _, err := os.Stat(filepath.Join(root, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings written outside native userdata path")
	}
	os.WriteFile(path, []byte(`not JSON`), 0600)
	if projectCursorInstance(root, "r_aaaaaa01", "reviewer") == nil {
		t.Fatal("corrupt settings silently replaced")
	}
}
func TestCursorThreadRequiresPersistedEnvironment(t *testing.T) {
	var home T3Home
	if _, err := home.readyForThread(context.Background(), t3Thread{Provider: "cursor"}); err == nil {
		t.Fatal("unbound Cursor thread accepted")
	}
	if _, err := home.readyForProvider(context.Background(), "", "", "unknown", ""); err == nil {
		t.Fatal("unknown driver fell back to shared T3")
	}
}

// Uses a disposable real runtime and the actual Rubi manager/resolver/pool. No
// provider turn or model API call is made; the Cursor key is a synthetic fixture.
func TestRealProjectT3PoolSmoke(t *testing.T) {
	native, relay := os.Getenv("RUBI_REAL_T3"), os.Getenv("RUBI_REAL_RELAY")
	if native == "" || relay == "" {
		t.Skip("disposable runtime/relay not supplied")
	}
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Skip("srt unavailable")
	}
	l, role := agentLife(t)
	version := filepath.Base(filepath.Dir(native))
	l.t3.Pin.Version = version
	dst := l.t3.exePath(version)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Dir(native), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(filepath.Dir(native), path)
		if err != nil {
			return err
		}
		target := filepath.Join(filepath.Dir(dst), rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		return os.Link(path, target)
	}); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(t.TempDir(), "cursor.json")
	os.WriteFile(credentials, []byte(`{"CURSOR_API_KEY":"synthetic-not-an-account"}`), 0600)
	real, err := filepath.EvalSymlinks(srt)
	if err != nil {
		t.Fatal(err)
	}
	install := agentsandbox.Installation{AgentExec: relay, SRT: srt, RuntimeRead: []string{filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))}, Providers: map[string]agentsandbox.Provider{"cursor": {CredentialsFile: credentials}}}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(dir) {
			if st, e := os.Stat(dir); e == nil && st.IsDir() {
				install.SearchPath = append(install.SearchPath, dir)
				if strings.HasPrefix(dir, "/workspace/") {
					install.RuntimeRead = append(install.RuntimeRead, dir)
				}
			}
		}
	}
	if err := l.h.Update(func(c *Config) { c.AgentSandbox = install }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cl, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", "")
	if err != nil {
		t.Fatal(err)
	}
	if cl == l.t3.client() {
		t.Fatal("Cursor used shared server")
	}
	if tools, err := cl.Tools(ctx); err != nil || len(tools) == 0 {
		t.Fatal("project server MCP unavailable (details suppressed)")
	}
	again, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", "")
	if err != nil || again != cl {
		t.Fatal("project server was not reused", err)
	}
	if err := l.h.Update(func(c *Config) {
		p := c.AgentSandbox.Providers["cursor"]
		p.AllowedDomains = []string{"github.com"}
		c.AgentSandbox.Providers["cursor"] = p
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", "")
	if err != nil || changed == cl {
		t.Fatal("changed network policy reused an old sandbox", err)
	}
	cl = changed
	hash := roles.DirHash(realPath(t, l.proj))
	state := l.t3.projectState(hash)
	marker := filepath.Join(state, "persisted")
	if err := os.WriteFile(marker, []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	l.t3.stopProjectServers()
	cl2, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", hash)
	if err != nil || cl2 == cl {
		t.Fatal("project server did not restart independently", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("project state lost on stop", err)
	}
	if _, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", "wrong-folder-binding"); err == nil {
		t.Fatal("thread could relocate to another environment")
	}
	other := filepath.Join(t.TempDir(), "other")
	os.Mkdir(other, 0700)
	if err := l.h.Update(func(c *Config) { c.AgentProjects = []string{other} }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.t3.readyForProvider(ctx, "my-project", role, "cursor", hash); err == nil {
		t.Fatal("removed project reused cached server")
	}
}

func TestProjectSettingsCannotFollowAgentSymlinkToHostSecrets(t *testing.T) {
	state, secretDir := t.TempDir(), t.TempDir()
	secret := filepath.Join(secretDir, "secret.json")
	original := []byte(`{"fixture_secret":"not-an-account"}`)
	os.WriteFile(secret, original, 0600)
	os.MkdirAll(filepath.Join(state, "t3", "userdata"), 0700)
	settings := filepath.Join(state, "t3", "userdata", "settings.json")
	os.Symlink(secret, settings)
	if projectCursorInstance(state, "r_aaaaaa01", "reviewer") == nil {
		t.Fatal("host secret symlink followed")
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != string(original) {
		t.Fatal("host secret changed")
	}
	os.Remove(settings)
	os.Remove(filepath.Join(state, "t3", "userdata"))
	os.Remove(filepath.Join(state, "t3"))
	os.Symlink(secretDir, filepath.Join(state, "t3"))
	if projectCursorInstance(state, "r_aaaaaa01", "reviewer") == nil {
		t.Fatal("ancestor directory escaped state root")
	}
	if _, err := os.Stat(filepath.Join(secretDir, "userdata")); !os.IsNotExist(err) {
		t.Fatal("host directory created outside state root")
	}
}

func TestCursorRefusalDoesNotCreateSharedThread(t *testing.T) {
	l, role := agentLife(t)
	_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "provider": "cursor", "model": "fixture", "prompt": "work"})
	if err == nil {
		t.Fatal("unprovisioned Cursor accepted")
	}
	if len(l.fake.Calls("t3_thread_launch")) != 0 || len(l.t3.threads.snapshot()) != 0 {
		t.Fatal("Cursor refusal created a shared thread")
	}
}
func TestEveryConfiguredCredentialIsHiddenAndCannotBeRuntimeReadable(t *testing.T) {
	b, c, path, _ := sandboxFixture(t)
	outside := t.TempDir()
	file := filepath.Join(outside, "provider.json")
	os.WriteFile(file, []byte(`{}`), 0600)
	c.AgentSandbox.Providers["cursor"] = agentsandbox.Provider{ProjectCredentials: map[string]string{"other": file}}
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	l, err := ResolveAgentLaunch(b, path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range l.Policy.SensitiveRoots {
		if p == file {
			found = true
		}
	}
	if !found {
		t.Fatal("other-project credential outside HOME is not hidden")
	}
	// Verify actual read denial as well as the resolver's metadata guard.
	if srt, e := exec.LookPath("srt"); e == nil {
		l.SRT = srt
		l.CLI = "/bin/sh"
		l.Args = []string{"-c", `value=$(cat "$1" 2>/dev/null); test -z "$value"`, "sh", file}
		real, e := filepath.EvalSymlinks(srt)
		if e != nil {
			t.Fatal(e)
		}
		l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real)))))
		l.SearchPath = nil
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if filepath.IsAbs(dir) {
				if st, e := os.Stat(dir); e == nil && st.IsDir() {
					l.SearchPath = append(l.SearchPath, dir)
					if strings.HasPrefix(dir, "/workspace/") {
						l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, dir)
					}
				}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		code, e := agentsandbox.Run(ctx, l, nil, nil, nil)
		if e != nil || code != 0 {
			t.Fatal("other-project credential content readable", e, code)
		}
	}
	c.AgentSandbox.RuntimeRead = append(c.AgentSandbox.RuntimeRead, outside)
	writeSandboxJSON(t, filepath.Join(b.HomeDir, "config.json"), c)
	if _, err := ResolveAgentLaunch(b, path); err == nil {
		t.Fatal("credential runtime exception accepted")
	}
}

func TestAutomaticProjectRuntimeCannotExposeOtherProviderCredentials(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "other.json")
	os.WriteFile(file, []byte(`{}`), 0600)
	install := agentsandbox.Installation{Providers: map[string]agentsandbox.Provider{"opencode": {ProjectCredentials: map[string]string{"b": file}}}}
	if checkCredentialReadGrants(install, []string{root}) == nil {
		t.Fatal("automatic runtime grant exposed another provider's credentials")
	}
}
