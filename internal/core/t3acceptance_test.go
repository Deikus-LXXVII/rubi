//go:build rubi_acceptance && linux

package core

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/home"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/plugins"
	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Every service is disposable. Browser/passkey gesture and model are fixtures;
// Core dispatcher/approval, actual companion subprocess, encrypted Home relay,
// native T3, srt/bwrap, Git, finalization, vault resolution and HTTP delivery run.
func TestT3AgentsCombinedAcceptance(t *testing.T) {
	native, helper, companion := os.Getenv("RUBI_REAL_T3"), os.Getenv("RUBI_REAL_RELAY"), os.Getenv("RUBI_T3_COMPANION_DIR")
	if native == "" || helper == "" || companion == "" {
		t.Skip("use scripts/test-t3-agents-acceptance.sh with native T3 and companion source")
	}
	for _, tool := range []string{"bwrap", "srt", "node", "setpriv", "unshare", "python3", "git", "socat", "rg"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("missing acceptance dependency", tool)
		}
	}
	if exec.Command("bwrap", "--unshare-user", "--ro-bind", "/", "/", "/bin/true").Run() != nil {
		t.Skip("bwrap/user namespaces unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "source", "my-project")
	mustMkdir(t, source)
	acceptanceGit(t, source, "init", "-b", "main")
	acceptanceGit(t, source, "config", "user.name", "Acceptance fixture")
	acceptanceGit(t, source, "config", "user.email", "fixture@localhost")
	mustWrite(t, filepath.Join(source, "base"), []byte("unchanged source fixture"), 0600)
	acceptanceGit(t, source, "add", "base")
	acceptanceGit(t, source, "commit", "-m", "initial fixture")
	base := acceptanceGit(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(root, "remote.git")
	mustMkdir(t, bare)
	acceptanceGit(t, bare, "init", "--bare", "-b", "main")
	acceptanceGit(t, source, "push", "--", bare, "HEAD:refs/heads/main")
	gh := &acceptanceGitHub{bare: bare, pulls: map[string]map[string]any{}}
	api := httptest.NewServer(gh)
	defer api.Close()
	received := make(chan map[string]any, 100)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		m["_fixture_path"] = r.URL.Path
		received <- m
		w.WriteHeader(200)
	}))
	defer receiver.Close()
	c, err := Open(paths.Layout{Home: filepath.Join(root, "core")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Lock() }()
	c.BotWebhookOK = localReceiver
	if err = c.Vault.Create(vault.NewKey(), vault.NewData("")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.paired = true
	c.mu.Unlock()
	c.SetEndpoint("https://acceptance.invalid")
	fixtureToken := "acceptance-not-a-real-credential"
	if err = c.Vault.Update(func(d *vault.Data) error {
		d.Integrations["t3agents"] = &vault.Integration{Enabled: true, Accounts: []*vault.Account{{ID: "acceptance", Label: "Local mock GitHub", Default: true, Agents: []string{"Mail"}, Secrets: map[string]string{"github_token": fixtureToken}}}}
		d.Agents = append(d.Agents, &vault.Agent{Name: "Mail", URL: receiver.URL + "/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066", Key: "fixture-receiver", Subscriptions: []string{"t3agents"}})
		d.Agents = append(d.Agents, &vault.Agent{Name: "Observer", Default: true, URL: receiver.URL + "/automations/webhook/6f8d7afd-0e49-48f3-94de-a33ec127108a", Key: "fixture-observer", Subscriptions: []string{"t3agents"}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Real Home pairing over encrypted Nostr frames through a local relay.
	rl := relaytest.New()
	defer rl.Close()
	h, err := home.Open(filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { h.T3.Stop(); h.Agent.Close() }()
	if err = h.Update(func(cfg *home.Config) {
		cfg.Relays = []string{rl.URL()}
		cfg.AgentProjects = []string{source}
		allow := true
		cfg.AgentAllowBypass = &allow
		zero := 0
		cfg.AgentMaxSessions = &zero
		cfg.AgentMaxSessionsPerProfile = 20
	}); err != nil {
		t.Fatal(err)
	}
	key, _ := h.RelayKey()
	ready := make(chan int, 8)
	var active atomic.Pointer[home.Helper]
	active.Store(h)
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: func(ctx context.Context, env homeproto.Envelope) (any, error) { return active.Load().Handle(ctx, env) }}
	go (&relay.Server{Key: key, Relays: h.Relays(), Handle: rpc.HandleRPC, Ready: func(n int) {
		select {
		case ready <- n:
		default:
		}
	}}).Run(ctx)
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("local Home relay did not start")
	}
	code, pin, err := h.PairingCode(false)
	if err != nil {
		t.Fatal("Home pairing fixture failed")
	}
	approval, err := c.PairDevice(ctx, code.String(), pin)
	if err != nil {
		t.Fatal("Home pair request failed")
	}
	if result, err := c.Approvals.DecidePanel(ctx, approval, "apply", true, approvals.Verified); err != nil || result.State != approvals.Executed {
		t.Fatal("Home pairing approval failed")
	}
	device := c.Devices()[0]["id"].(string)
	// An unmodified native T3 binary, not a mock server or database implementation.
	tools := filepath.Join(root, "tools")
	mustMkdir(t, tools)
	script, err := os.ReadFile("../home/testdata/task-codex.py")
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(tools, "codex")
	mustWrite(t, fake, script, 0700)
	other := filepath.Join(root, "project-b")
	mustMkdir(t, other)
	secret := filepath.Join(h.Dir, "home-secret-fixture")
	marker := filepath.Join(other, "marker")
	for _, path := range []string{secret, marker} {
		mustWrite(t, path, []byte("outside fixture"), 0600)
	}
	probes, _ := json.Marshal([]string{secret, marker, filepath.Join(source, "base")})
	mustWrite(t, filepath.Join(tools, "probe-paths.json"), probes, 0600)
	node, _ := exec.LookPath("node")
	srt, _ := exec.LookPath("srt")
	real, _ := filepath.EvalSymlinks(srt)
	pathJSON, _ := json.Marshal(native)
	launcher := filepath.Join(tools, "native.cjs")
	mustWrite(t, launcher, []byte("const c=require('child_process').spawn("+string(pathJSON)+",process.argv.slice(2),{stdio:'inherit'});c.on('exit',x=>process.exit(x??1));"), 0600)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	server := &t3.Server{Home: filepath.Join(root, "native-profile"), Node: node, Bin: launcher, BaseDir: filepath.Join(root, "native-state"), RuntimeHome: filepath.Join(root, "native-home"), Port: port}
	mustMkdir(t, server.Home)
	profileSecret := filepath.Join(server.Home, "private-account-fixture")
	mustWrite(t, profileSecret, []byte("outside fixture"), 0600)
	probes, _ = json.Marshal([]string{secret, marker, filepath.Join(source, "base"), profileSecret})
	mustWrite(t, filepath.Join(tools, "probe-paths.json"), probes, 0600)
	defer server.Stop()
	if _, err = server.Start(ctx); err != nil {
		t.Fatal("native T3 start failed (output suppressed)")
	}
	nativeCode, err := server.PairingCode(ctx, t3.PairingScopes("full-access"), time.Minute, "acceptance")
	if err != nil {
		t.Fatal("native pair code failed")
	}
	token, err := t3.Pair(ctx, http.DefaultClient, server.Base(), nativeCode, "full-access", "acceptance")
	if err != nil {
		t.Fatal("native pairing failed")
	}
	install := agentsandbox.Installation{AgentExec: helper, SRT: srt, RuntimeRead: []string{filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))}, Providers: map[string]agentsandbox.Provider{"codex": {CLI: fake, RuntimeRead: []string{tools}}}}
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
	if err = h.Update(func(cfg *home.Config) {
		cfg.T3BaseDir = server.BaseDir
		cfg.T3Port = port
		cfg.AgentSandbox = install
		cfg.AgentProjects = append(cfg.AgentProjects, other)
	}); err != nil {
		t.Fatal(err)
	}
	home.AcceptanceNative(h, server, t3.New(server.Base(), token, t3.FullAccess), ctx)
	home.AcceptancePublication(h, bare, api.URL)
	var put map[string]any
	dev, _ := c.device(device)
	client, err := c.deviceClient(dev)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Call(ctx, "t3-agent.role.put", map[string]any{"base_version": 0, "by": "acceptance", "spec": map[string]any{"project": "my-project", "name": "acceptance", "description": "disposable task role", "tools": []string{"Read", "Write", "Edit", "Bash"}}}, &put); err != nil {
		t.Fatal("role creation failed", err)
	}
	role := put["id"].(string)
	// Build the actual companion. Only a generated test PUBLIC key is injected;
	// no real publisher private key, account credential or release is accessed.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	pubText := plugintest.KeyB64(pub)
	t.Setenv("RUBI_TEST_PLUGIN_KEY", pubText)
	stage := filepath.Join(root, "plugin-stage")
	mustMkdir(t, stage)
	binary := filepath.Join(stage, "t3agents")
	build := exec.CommandContext(ctx, "go", "build", "-ldflags=-X main.PublisherKey="+pubText, "-o", binary, ".")
	build.Dir = companion
	if build.Run() != nil {
		t.Fatal("companion build failed (output suppressed)")
	}
	raw, err := exec.Command(binary, "--manifest").Output()
	if err != nil {
		t.Fatal("companion manifest failed")
	}
	var m plugins.Manifest
	err = json.Unmarshal(raw, &m)
	if err == nil {
		err = plugins.Check(&m)
	}
	if err != nil {
		t.Fatal("companion manifest invalid", err)
	}
	mustWrite(t, filepath.Join(stage, "manifest.json"), raw, 0600)
	tree, err := plugins.TreeHash(stage)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &plugins.Candidate{Manifest: m, Dir: stage, Tree: tree, Source: m.Source, Reviewed: false}
	if err = c.Store.Commit(candidate, ""); err != nil {
		t.Fatal(err)
	}
	if err = c.Vault.Update(func(d *vault.Data) error {
		d.Plugins[m.ID] = &vault.Plugin{Version: m.Version, Source: m.Source, PublisherKey: m.Publisher.Key, Tree: tree}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = c.Runner.Start(m.ID, m.Version, tree); err != nil {
		t.Fatal("real companion start failed", err)
	}
	defer c.Runner.StopAll()
	// Account assignment is a fixture prerequisite. The real multi-Bot access
	// broker grants its scoped code without a second user confirmation.
	grant, e := c.RequestAccess(ctx, "Mail", []AccessResource{{Plugin: "t3agents", Account: "acceptance"}}, "disposable acceptance tasks", 60)
	if e != nil || grant["status"] == "awaiting_approval" {
		t.Fatal("preassigned fixture account was not granted")
	}
	accessCode := ""
	until := time.NewTimer(10 * time.Second)
	defer until.Stop()
	for accessCode == "" {
		select {
		case m := <-received:
			if data, ok := m["data"].(map[string]any); ok {
				accessCode, _ = data["access_code"].(string)
			}
		case <-until.C:
			t.Fatal("scoped fixture access code not delivered")
		}
	}

	taskDir := func(id string) string {
		t.Helper()
		w, e := agenttask.Resolve(h.Dir, id, "my-project", role, roles.DirHash(source))
		if e != nil {
			t.Fatal(e)
		}
		return w.Dir
	}
	start := func(prompt string, publish bool, lost ...bool) (string, string) {
		t.Helper()
		args := map[string]any{"rubi_agent": "Mail", "rubi_agent_code": c.agentCodeFor("Mail"), "rubi_access": accessCode, "device": device, "project": "my-project", "role": role, "prompt": prompt, "provider": "codex", "model": "gpt-5.4"}
		if publish {
			args["publish"] = map[string]string{"repository": "owner/repo", "base": "main", "account": "acceptance"}
		}
		out, e := c.CallTool(ctx, "t3agents_task_start", acceptanceJSON(args))
		if e != nil {
			t.Fatal("task tool failed", e)
		}
		id, ok := out["approval_id"].(string)
		if !ok {
			t.Fatalf("task did not request exactly one confirmation (status %v)", out["status"])
		}
		detail, _ := c.Approvals.Detail(id)
		if detail.Kind != "t3agents.task_start" || detail.Level != approvals.Strong {
			t.Fatal("wrong task confirmation")
		}
		result, e := c.Approvals.DecidePanel(ctx, id, "start", true, approvals.Verified)
		want := approvals.Executed
		if len(lost) > 0 && lost[0] {
			want = approvals.Failed
		}
		if e != nil || result.State != want {
			t.Fatal("task approval execution failed", result.Error, e)
		}
		states, _ := (agenttask.Store{Home: h.Dir}).List(ctx)
		for _, state := range states {
			if strings.HasPrefix(state.Spec.Prompt, prompt+"\n\nRubi repository task:") {
				return state.ID, id
			}
		}
		t.Fatal("task mandate missing after approved start")
		return "", ""
	}
	task, _ := start("Edit task-edit.txt and commit", true)
	st := agenttask.Store{Home: h.Dir}
	deadline := time.Now().Add(100 * time.Second)
	var finished agenttask.State
	for time.Now().Before(deadline) {
		finished, _ = st.Get(ctx, task)
		if finished.Phase.Terminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finished.Phase != agenttask.Finished {
		_, items, _ := t3.New(server.Base(), token, t3.FullAccess).Read(ctx, finished.Thread, 0)
		for _, item := range items {
			if item.Type != "message" && item.Type != "assistant_message" {
				t.Log("fixture native item", item.Type, item.Text)
			}
		}
		for _, name := range []string{"fixture-methods", "fixture-error"} {
			if b, e := os.ReadFile(filepath.Join(taskDir(finished.ID), name)); e == nil {
				t.Log(name, string(b))
			}
		}
		if b, e := os.ReadFile(server.LogFile()); e == nil {
			for _, line := range strings.Split(string(b), "\n") {
				for _, kind := range []string{"rubi-agent-exec:", "ENOENT", "EACCES", "Provider process exited", "sandbox:"} {
					if at := strings.Index(line, kind); at >= 0 {
						t.Log("fixture native failure", line[at:])
						break
					}
				}
			}
		}
		t.Fatalf("automatic completion stalled in %s (%s)", finished.Phase, finished.Error)
	}
	if finished.PR != "https://github.com/owner/repo/pull/1" {
		t.Fatal("PR receipt missing")
	}
	if acceptanceGit(t, bare, "rev-parse", "refs/heads/main") != base {
		t.Fatal("publication changed main")
	}
	if acceptanceGit(t, bare, "rev-parse", "refs/heads/"+finished.Workspace.Branch) != finished.Commit {
		t.Fatal("exact task ref not pushed")
	}
	if acceptanceGit(t, source, "rev-parse", "HEAD") != base {
		t.Fatal("source branch changed")
	}
	for _, p := range []string{secret, marker, profileSecret} {
		b, _ := os.ReadFile(p)
		if string(b) != "outside fixture" {
			t.Fatal("sandbox fixture changed")
		}
	}
	card, err := c.PanelCall(ctx, "t3agents", "view", acceptanceJSON(map[string]any{"page": "tasks", "device": device}))
	if err != nil {
		t.Fatal("task panel card failed", err)
	}
	cardJSON, _ := json.Marshal(card)
	if strings.Contains(string(cardJSON), fixtureToken) {
		t.Fatal("credential leaked to task card")
	}
	if !strings.Contains(string(cardJSON), finished.PR) || !strings.Contains(string(cardJSON), "agent-reported") {
		t.Fatal("panel omitted PR/test provenance")
	}

	// Verify actual requester-only task_result, not an unrelated approval notice.
	awaitMessage := func(id string) {
		t.Helper()
		timer := time.NewTimer(45 * time.Second)
		defer timer.Stop()
		for {
			select {
			case m := <-received:
				raw, _ := json.Marshal(m)
				if strings.Contains(string(raw), id) && strings.Contains(string(raw), "task_result") {
					if m["_fixture_path"] != "/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066" {
						t.Fatal("task result leaked to another subscribed/default Bot")
					}
					if !strings.Contains(string(raw), "https://rubi-panel.com/") {
						t.Fatal("requester result omitted full panel link")
					}
					if strings.Contains(string(raw), fixtureToken) {
						t.Fatal("credential leaked to requester")
					}
					return
				}
			case <-timer.C:
				t.Fatal("requester task_result missing")
			}
		}
	}
	awaitMessage(task)
	waitPhase := func(id string, want agenttask.Phase) agenttask.State {
		t.Helper()
		until := time.Now().Add(90 * time.Second)
		for time.Now().Before(until) {
			state, e := st.Get(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if state.Phase == want {
				if state.Phase.Terminal() {
					out, e := c.CallTool(ctx, "t3agents_task_status", acceptanceJSON(map[string]any{"rubi_agent": "Mail", "rubi_agent_code": c.agentCodeFor("Mail"), "rubi_access": accessCode, "device": device, "id": id}))
					if e != nil {
						t.Fatal("MCP task status failed", e)
					}
					b, _ := json.Marshal(out)
					if json.Unmarshal(b, &state) != nil || state.ID != id {
						t.Fatal("MCP status shape mismatch")
					}
					if !state.ExecutionStopped {
						time.Sleep(50 * time.Millisecond)
						continue
					}
				}
				return state
			}
			if state.Phase.Terminal() && state.Phase != want {
				t.Fatalf("unexpected terminal phase %s", state.Phase)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("task phase deadline", want)
		return agenttask.State{}
	}
	control := func(op, id, text string) {
		t.Helper()
		_, e := c.CallTool(ctx, "t3agents_task_"+op, acceptanceJSON(map[string]any{"rubi_agent": "Mail", "rubi_agent_code": c.agentCodeFor("Mail"), "rubi_access": accessCode, "device": device, "id": id, "text": text}))
		if e != nil {
			t.Fatal("real companion control failed", op, e)
		}
	}
	waitFile := func(dir, name, contains string) {
		t.Helper()
		until := time.Now().Add(30 * time.Second)
		for time.Now().Before(until) {
			b, e := os.ReadFile(filepath.Join(dir, name))
			if e == nil && strings.Contains(string(b), contains) {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		states, _ := st.List(ctx)
		for _, state := range states {
			if taskDir(state.ID) == dir {
				t.Log("checkpoint task phase", state.Phase, state.Error)
				_, items, _ := t3.New(server.Base(), token, t3.FullAccess).Read(ctx, state.Thread, 0)
				for _, item := range items {
					if item.Type != "message" && item.Type != "assistant_message" {
						t.Log("checkpoint native", item.Type, item.Text)
					}
				}
			}
		}
		if b, e := os.ReadFile(server.LogFile()); e == nil {
			for _, line := range strings.Split(string(b), "\n") {
				for _, kind := range []string{"rubi-agent-exec:", "ENOENT", "EACCES", "Provider process exited", "sandbox:"} {
					if at := strings.Index(line, kind); at >= 0 {
						t.Log("checkpoint failure", line[at:])
						break
					}
				}
			}
		}
		t.Fatal("sandbox fixture did not reach checkpoint", name)
	}
	stableHeartbeat := func(dir string) {
		t.Helper()
		path := filepath.Join(dir, "fixture-heartbeat")
		a, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		time.Sleep(300 * time.Millisecond)
		b, e := os.ReadFile(path)
		if e != nil || !bytes.Equal(a, b) {
			t.Fatal("sandbox descendant survived confirmed cancellation")
		}
	}
	held, _ := start("hold-fixture append-and-stop", false)
	hs := waitPhase(held, agenttask.Running)
	waitFile(taskDir(hs.ID), "fixture-held", "held")
	control("append", held, "append from phone")
	waitFile(taskDir(hs.ID), "fixture-methods", "turn/steer")
	control("stop", held, "")
	hs = waitPhase(held, agenttask.Stopped)
	if !hs.ExecutionStopped {
		t.Fatal("stop acknowledged without drain")
	}
	stableHeartbeat(taskDir(hs.ID))
	awaitMessage(held)
	failed, _ := start("fail-fixture requester failure", false)
	fs := waitPhase(failed, agenttask.Failed)
	if !fs.ExecutionStopped {
		t.Fatal("failure did not drain execution")
	}
	awaitMessage(failed)
	resumed, _ := start("hold-fixture restart", true)
	rs := waitPhase(resumed, agenttask.Running)
	waitFile(taskDir(rs.ID), "fixture-held", "held")
	home.AcceptanceCrash(h)
	h, err = home.Open(h.Dir)
	if err != nil {
		t.Fatal(err)
	}
	active.Store(h)
	home.AcceptanceNative(h, server, t3.New(server.Base(), token, t3.FullAccess), ctx)
	control("append", resumed, "finish-fixture after daemon restart")
	rs = waitPhase(resumed, agenttask.Finished)
	if rs.Thread == "" || rs.PR != "https://github.com/owner/repo/pull/2" {
		t.Fatal("restart lost native thread or publication receipt")
	}
	awaitMessage(resumed)
	// A real accepted launch loses only its response. The requester can stop its
	// durable task from the panel without knowing the native thread ID.
	lost := &acceptanceLostReply{base: http.DefaultTransport}
	home.AcceptanceCrash(h)
	h, err = home.Open(h.Dir)
	if err != nil {
		t.Fatal(err)
	}
	active.Store(h)
	home.AcceptanceNative(h, server, t3.NewWithTransport(server.Base(), token, t3.FullAccess, lost), ctx)

	lost.beforeMask = func() error {
		until := time.Now().Add(30 * time.Second)
		for time.Now().Before(until) {
			states, _ := st.List(ctx)
			for _, state := range states {
				if strings.HasPrefix(state.Spec.Prompt, "hold-fixture unknown-receipt") {
					w, e := agenttask.Resolve(h.Dir, state.ID, "my-project", role, roles.DirHash(source))
					if e == nil {
						if b, e := os.ReadFile(filepath.Join(w.Dir, "fixture-heartbeat")); e == nil && len(b) > 0 {
							return nil
						}
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		return fmt.Errorf("fixture accepted worker did not reach heartbeat checkpoint")
	}

	unknown, _ := start("hold-fixture unknown-receipt", false, true)
	us, e := st.Get(ctx, unknown)
	if e != nil || us.Thread != "" || !lost.dropped.Load() {
		t.Fatal("receipt-loss fixture did not preserve an unknown launch")
	}
	control("stop", unknown, "")
	us = waitPhase(unknown, agenttask.Stopped)
	if !us.ExecutionStopped {
		t.Fatal("unknown receipt stop unconfirmed")
	}
	stableHeartbeat(taskDir(us.ID))
	awaitMessage(unknown)
	if _, e = st.Admit(ctx, unknown); e == nil {
		t.Fatal("late launcher ignored durable stop fence")
	}

	refs := strings.Fields(acceptanceGit(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads"))
	allowed := map[string]bool{"refs/heads/main": true, "refs/heads/" + finished.Workspace.Branch: true, "refs/heads/" + rs.Workspace.Branch: true}
	if len(refs) != 3 {
		t.Fatal("publication pushed unexpected branch count")
	}
	for _, ref := range refs {
		if !allowed[ref] {
			t.Fatal("publication pushed an unauthorized ref")
		}
	}
	if acceptanceGit(t, bare, "rev-parse", "refs/heads/"+rs.Workspace.Branch) != rs.Commit {
		t.Fatal("restart published a different commit")
	}

	gh.mu.Lock()
	count := len(gh.pulls)
	gh.mu.Unlock()
	if count != 2 {
		t.Fatal("publication retry duplicated a PR")
	}

	t.Log("PASS: real companion/Core one-confirmation -> encrypted Home -> native T3 -> srt/bwrap fake Codex -> commit -> exact bare push -> mock GitHub PR -> real card and requester HTTP delivery")
}
func acceptanceJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if e := os.MkdirAll(p, 0700); e != nil {
		t.Fatal(e)
	}
}
func mustWrite(t *testing.T, p string, b []byte, m os.FileMode) {
	t.Helper()
	if e := os.WriteFile(p, b, m); e != nil {
		t.Fatal(e)
	}
}
func acceptanceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	b, e := cmd.Output()
	if e != nil {
		t.Fatal("local fixture Git failed")
	}
	return strings.TrimSpace(string(b))
}

type acceptanceGitHub struct {
	bare  string
	mu    sync.Mutex
	pulls map[string]map[string]any
}

func (g *acceptanceGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer acceptance-not-a-real-credential" {
		http.Error(w, "fixture auth refused", 401)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/repos/owner/repo" {
		io.WriteString(w, `{"default_branch":"main"}`)
		return
	}
	if r.URL.Path != "/repos/owner/repo/pulls" {
		http.Error(w, "unexpected endpoint", 400)
		return
	}
	if r.Method == "GET" {
		branch := strings.TrimPrefix(r.URL.Query().Get("head"), "owner:")
		if p := g.pulls[branch]; p != nil {
			json.NewEncoder(w).Encode([]any{p})
		} else {
			io.WriteString(w, "[]")
		}
		return
	}
	if r.Method != "POST" {
		http.Error(w, "merge/other mutation refused", 400)
		return
	}
	var in map[string]string
	if json.NewDecoder(r.Body).Decode(&in) != nil || !strings.HasPrefix(in["head"], "rubi/task_") || in["base"] != "main" {
		http.Error(w, "wrong scope", 400)
		return
	}
	cmd := exec.Command("git", "-C", g.bare, "rev-parse", "refs/heads/"+in["head"])
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + g.bare, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	raw, e := cmd.Output()
	if e != nil {
		http.Error(w, "branch missing", 400)
		return
	}
	commit := strings.TrimSpace(string(raw))
	n := len(g.pulls) + 1
	p := map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/owner/repo/pull/%d", n), "head": map[string]any{"ref": in["head"], "sha": commit, "repo": map[string]string{"full_name": "owner/repo"}}, "base": map[string]string{"ref": "main"}}
	g.pulls[in["head"]] = p
	json.NewEncoder(w).Encode(p)
}

type acceptanceLostReply struct {
	beforeMask func() error
	base       http.RoundTripper
	dropped    atomic.Bool
}

func (l *acceptanceLostReply) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return l.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	resp, err := l.base.RoundTrip(req)
	if err == nil && envelope.Method == "tools/call" && envelope.Params.Name == "t3_thread_launch" && l.dropped.CompareAndSwap(false, true) {
		if l.beforeMask != nil {
			if e := l.beforeMask(); e != nil {
				resp.Body.Close()
				return nil, e
			}
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// Mask the accepted result with an RPC error, not a transport teardown:
		// closing the MCP session itself can cancel the native launch before it runs.
		replacement := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"fixture: accepted launch receipt unavailable"}}`, envelope.ID))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = int64(len(replacement))
		resp.Body = io.NopCloser(bytes.NewReader(replacement))
		return resp, nil
	}
	return resp, err
}
