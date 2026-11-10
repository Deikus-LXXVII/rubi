package home

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/hostguard"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"github.com/Deikus-LXXVII/rubi/internal/t3/t3test"
)

// fakeServer stands in for the T3 server process.
type fakeServer struct {
	mu                      sync.Mutex
	alive                   bool
	base, version           string
	pid                     int
	startErr, codeErr       error
	started, stopped, codes int
	scopes                  []string
	node, bin, dir, rtHome  string
	port                    int
	// What Adopt finds: a left server of adoptVer when adoptOK.
	adoptOK  bool
	adoptVer string
	adoptErr error
	adopts   int
}

func (f *fakeServer) Adopt(context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adopts++
	if f.adoptErr != nil || !f.adoptOK {
		return "", false, f.adoptErr
	}
	f.alive = true
	return f.adoptVer, true, nil
}

func (f *fakeServer) Start(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.alive = true
	f.started++
	return f.version, nil
}
func (f *fakeServer) Stop()           { f.mu.Lock(); f.alive = false; f.stopped++; f.mu.Unlock() }
func (f *fakeServer) Pid() int        { f.mu.Lock(); defer f.mu.Unlock(); return f.pid }
func (f *fakeServer) Alive() bool     { f.mu.Lock(); defer f.mu.Unlock(); return f.alive }
func (f *fakeServer) Base() string    { return f.base }
func (f *fakeServer) Version() string { return f.version }
func (f *fakeServer) PairingCode(_ context.Context, scopes []string, _ time.Duration, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes++
	f.scopes = scopes
	if f.codeErr != nil {
		return "", f.codeErr
	}
	return "PAIR-CODE-1", nil
}

// life is a Home with a fake T3 server and a fake T3 MCP behind it.
type life struct {
	h     *Helper
	t3    *T3Home
	fake  *t3test.Fake
	srv   *fakeServer
	kit   *installKit
	proj  string
	picks int
	port  int

	mu       sync.Mutex
	pairs    []string // "code access" of each trade
	served   int
	serveCtx context.Context
}

func newLife(t *testing.T) *life {
	t.Helper()
	h, _, proj := agentHelper(t)
	h.Agent.Close()
	h.vmOverride = func(agent.Project) agent.VM { return &agent.Fake{} }
	h.Agent = h.newAgent() // the real wiring of the limits and usage modes, with VMs that do nothing
	t.Cleanup(h.Agent.Close)
	l := &life{h: h, t3: h.T3, fake: t3test.New(t), kit: newKit(t), proj: proj}
	l.srv = &fakeServer{base: l.fake.URL(), version: testVer, pid: 4242}
	th := h.T3
	th.Fetch, th.Pin, th.Platform = l.kit.fetch, &l.kit.pin, func() string { return testPlat }
	th.MemAvailable = func() (int, bool) { return 0, false } // unknown: the floor doesn't apply
	th.NewServer = func(node, bin, base, rt string, port int) t3Server {
		l.srv.node, l.srv.bin, l.srv.dir, l.srv.rtHome, l.srv.port = node, bin, base, rt, port
		return l.srv
	}
	th.Pair = func(_ context.Context, base, code, access string) (t3.Token, error) {
		l.mu.Lock()
		l.pairs = append(l.pairs, code+" "+access)
		l.mu.Unlock()
		return t3.Token{Access: t3test.Token, Scopes: t3.PairingScopes(access), Expires: time.Now().Add(30 * 24 * time.Hour), ClientID: "c1"}, nil
	}
	th.PickPort = func() (int, error) {
		l.picks++
		if l.port != 0 {
			return l.port, nil
		}
		return freePort()
	}

	th.AgentSetup = func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		return map[string]any{"steps": []any{}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	th.base = ctx
	t.Cleanup(func() { l.fake.Release(); th.Stop(); cancel() })
	return l
}

func (l *life) install(t *testing.T) {
	t.Helper()
	if err := l.t3.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// up installs and starts.
func (l *life) up(t *testing.T) {
	t.Helper()
	l.install(t)
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestT3StartBringsEverythingUp(t *testing.T) {
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	l.install(t)
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := l.h.Fresh()
	if c.T3Port == 0 || c.T3Port == t3.UserPort || c.T3Port != l.srv.port {
		t.Errorf("port %d (server got %d)", c.T3Port, l.srv.port)
	}
	if c.T3BaseDir != filepath.Join(l.h.Dir, "t3", "home") || l.srv.dir != c.T3BaseDir {
		t.Errorf("base dir %q (server got %q)", c.T3BaseDir, l.srv.dir)
	}
	if l.srv.node != l.t3.nodeBin() || l.srv.bin != l.t3.pkgBin() || l.srv.rtHome != l.t3.runtimeHome() {
		t.Errorf("server started with %s %s %s", l.srv.node, l.srv.bin, l.srv.rtHome)
	}
	// Paired at full access (decision 1), with a code minted on this computer, and the token kept private.
	if !reflect.DeepEqual(l.srv.scopes, t3.PairingScopes("full-access")) {
		t.Errorf("pairing scopes %v", l.srv.scopes)
	}
	if len(l.pairs) != 1 || l.pairs[0] != "PAIR-CODE-1 full-access" {
		t.Errorf("trades: %v", l.pairs)
	}
	st, err := os.Stat(l.t3.tokenFile())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", st, err)
	}
	b, _ := os.ReadFile(l.t3.tokenFile())
	if tok, err := t3.LoadToken(b); err != nil || tok.Access != t3test.Token {
		t.Errorf("saved token: %v", err)
	}
	// The client's calls carry the bridge ceiling.
	if _, err := l.t3.client().Projects(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := l.fake.Calls("t3_project_list")
	if got := calls[len(calls)-1].Header.Get("t3-mode-limit"); got != "full-access/default" {
		t.Errorf("mode limit %q", got)
	}
	// The role instances are in T3's settings, the session socket is served, and only T3's children are let in.
	sb, err := os.ReadFile(filepath.Join(c.T3BaseDir, "userdata", "settings.json"))
	if err != nil || !strings.Contains(string(sb), "claude-"+role) {
		t.Errorf("settings: %v %s", err, sb)
	}
	if l.served != 0 {
		t.Fatal("native startup launched a retired VM session server")
	}
	if l.h.T3Pid() != 4242 {
		t.Errorf("T3Pid = %d", l.h.T3Pid())
	}
	// Starting again changes nothing.
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.started != 1 || len(l.pairs) != 1 || l.served != 0 {
		t.Errorf("a second start did work: started %d, trades %d, served %d", l.srv.started, len(l.pairs), l.served)
	}
	// Stop ends the session server and the server, and the pid is gone.
	l.t3.Stop()
	if l.srv.stopped != 1 || l.h.T3Pid() != 0 || l.t3.srvAlive() || l.t3.client() != nil {
		t.Errorf("after stop: stopped %d, pid %d", l.srv.stopped, l.h.T3Pid())
	}

}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func putRole(t *testing.T, h *Helper, name string) string {
	t.Helper()
	spec := map[string]any{"project": "my-project", "name": name, "description": "d", "tools": []string{"Read"}}
	put, err := handle(t, h, "tok", "t3-agent.role.put", map[string]any{"base_version": 0, "spec": spec, "by": "bot-a"})
	if err != nil {
		t.Fatal(err)
	}
	return put["id"].(string)
}

func TestT3StartRefusals(t *testing.T) {
	l := newLife(t)
	if err := l.t3.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "install") {
		t.Errorf("start before the install: %v", err)
	}
	l.install(t)

	l.srv.version = "0.0.1-other"
	if err := l.t3.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "pinned one is "+testVer) {
		t.Errorf("a server of another version: %v", err)
	}
	if l.srv.alive || l.t3.srvAlive() {
		t.Error("a server of another version was kept")
	}
	l.srv.version = testVer

	l.srv.startErr = errors.New("boom")
	if err := l.t3.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a server that does not start: %v", err)
	}
	l.srv.startErr = nil

	// The user's own T3 port is never chosen.
	l.h.Update(func(c *Config) { c.T3Port = 0 })
	l.port = t3.UserPort
	if err := l.t3.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "own T3") {
		t.Errorf("port 3773: %v", err)
	}
	if got := l.h.Fresh().T3Port; got == t3.UserPort {
		t.Error("port 3773 was recorded")
	}
}

func TestT3PortIsChosenOnceAndKept(t *testing.T) {
	l := newLife(t)
	l.install(t)
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := l.h.Fresh().T3Port
	l.t3.Stop()
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.picks != 1 || l.h.Fresh().T3Port != first {
		t.Errorf("picked %d times, port %d then %d", l.picks, first, l.h.Fresh().T3Port)
	}
	// A recorded 3773 (the user's own T3) is never kept, even when it is free.
	l.t3.Stop()
	if err := l.h.Update(func(c *Config) { c.T3Port = t3.UserPort }); err != nil {
		t.Fatal(err)
	}
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := l.h.Fresh().T3Port; got == t3.UserPort || l.srv.port == t3.UserPort {
		t.Errorf("kept port %d", got)
	}
	picks := l.picks
	// A port someone else holds now is replaced, and the new one is recorded.
	l.t3.Stop()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	if err := l.h.Update(func(c *Config) { c.T3Port = busy }); err != nil {
		t.Fatal(err)
	}
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := l.h.Fresh().T3Port; got == busy || got == 0 || l.srv.port != got || l.picks != picks+1 {
		t.Errorf("port %d after the busy one %d (server %d, picks %d)", got, busy, l.srv.port, l.picks)
	}
}

func TestT3PairingIsKeptUntilItStopsWorking(t *testing.T) {
	l := newLife(t)
	l.up(t)
	l.t3.Stop()
	// A saved token that works is used: no new pairing.
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.codes != 1 || len(l.pairs) != 1 {
		t.Fatalf("paired again with a good token: %d %v", l.srv.codes, l.pairs)
	}
	l.t3.Stop()

	save := func(tok t3.Token) {
		b, _ := tok.Save()
		if err := os.WriteFile(l.t3.tokenFile(), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A token that T3 does not know (its home was reset) is paired anew.
	save(t3.Token{Access: "not-known-to-t3", Expires: time.Now().Add(20 * 24 * time.Hour)})
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.pairs) != 2 {
		t.Errorf("a token T3 refused was kept: %v", l.pairs)
	}
	l.t3.Stop()
	// A token with less than a day left is paired anew, before it fails.
	save(t3.Token{Access: t3test.Token, Expires: time.Now().Add(time.Hour)})
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.pairs) != 3 {
		t.Errorf("a token about to expire was kept: %v", l.pairs)
	}
	l.t3.Stop()
	// A garbled file is paired anew too.
	if err := os.WriteFile(l.t3.tokenFile(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.pairs) != 4 {
		t.Errorf("a garbled token file was accepted: %v", l.pairs)
	}
}

func TestT3PairingFailureLeavesAServerAndAPlainError(t *testing.T) {
	l := newLife(t)
	l.install(t)
	l.srv.codeErr = errors.New("no pairing today")
	err := l.t3.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pairing failed") || !strings.Contains(err.Error(), "no pairing today") {
		t.Fatalf("%v", err)
	}
	if !l.t3.srvAlive() || l.t3.client() != nil {
		t.Error("expected a running server and no client")
	}
	if _, err := l.t3.ready(); err == nil || !strings.Contains(err.Error(), "pair step") {
		t.Errorf("ready: %v", err)
	}
	st := l.t3.Status()
	if st["running"] != true || st["paired"] != false {
		t.Errorf("status: running %v paired %v", st["running"], st["paired"])
	}
	// The pair step run by hand works once the cause is gone.
	l.srv.codeErr = nil
	if err := l.t3.RunStep(T3StepPair); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	if l.t3.client() == nil {
		t.Errorf("not paired after the pair step: %v", l.t3.Status()["error"])
	}
}

func TestT3ReadyNamesTheStepToRun(t *testing.T) {
	l := newLife(t)
	if _, err := l.t3.ready(); err == nil || !strings.Contains(err.Error(), "install step") {
		t.Errorf("not installed: %v", err)
	}
	l.install(t)
	if _, err := l.t3.ready(); err == nil || !strings.Contains(err.Error(), "start step") {
		t.Errorf("not running: %v", err)
	}
	l.srv.alive = true
	l.t3.mu.Lock()
	l.t3.srv = l.srv
	l.t3.mu.Unlock()
	if _, err := l.t3.ready(); err == nil || !strings.Contains(err.Error(), "pair step") {
		t.Errorf("not paired: %v", err)
	}
}

// A server that has died is not a running server, whatever the client says.
func TestT3DeadServerIsNotReady(t *testing.T) {
	l := newLife(t)
	l.up(t)
	l.srv.mu.Lock()
	l.srv.alive = false
	l.srv.mu.Unlock()
	if l.t3.client() != nil {
		t.Error("a client for a dead server")
	}
	if _, err := l.t3.ready(); err == nil || !strings.Contains(err.Error(), "start step") {
		t.Errorf("%v", err)
	}
}

// ---- t3-setup.* ----

func TestT3SetupStatusShape(t *testing.T) {
	l := newLife(t)
	putRole(t, l.h, "reviewer")
	l.t3.AgentSetup = func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		return map[string]any{"steps": []any{map[string]any{"id": "container"}, map[string]any{"id": "image"}, map[string]any{"id": "projects"},
			map[string]any{"id": "login"}, map[string]any{"id": "selftest"}}, "job": nil, "login": nil}, nil
	}
	st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st["installed"] != false || st["running"] != false || st["paired"] != false || st["version"] != testVer || st["port"].(float64) != 0 {
		t.Fatalf("status before: %v", st)
	}
	ids := func(st map[string]any) string {
		var out []string
		for _, s := range st["steps"].([]any) {
			out = append(out, s.(map[string]any)["id"].(string))
		}
		return strings.Join(out, ",")
	}
	// T3's own steps sit after the image, the agent setup's steps keep their order.
	if got := ids(st); got != "t3,pair,projects,login,selftest" {
		t.Errorf("steps: %s", got)
	}
	l.up(t)
	st, err = handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st["installed"] != true || st["running"] != true || st["paired"] != true || st["installed_version"] != testVer || st["server_version"] != testVer ||
		int(st["port"].(float64)) != l.srv.port {
		t.Errorf("status after: %v", st)
	}
	inst, _ := st["instances"].([]any)
	if len(inst) != 1 || !strings.HasPrefix(inst[0].(string), "claude-") {
		t.Errorf("instances: %v", st["instances"])
	}
	for _, s := range st["steps"].([]any) {
		m := s.(map[string]any)
		switch m["id"] {
		case "t3", "pair":
			if m["state"] != "done" {
				t.Errorf("step %v is %v", m["id"], m["state"])
			}
		}
	}

	// Retired VM sign-in is not offered, even when its old credential file exists.
	login := stepByID(t, st, "login")
	if login["state"] != "unsupported" || login["action"] != "" || !strings.Contains(login["reason"].(string), "retired") {
		t.Errorf("native provider provisioning: %v", login)
	}
	if err := os.MkdirAll(filepath.Join(l.h.Dir, "agent-signin"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.h.Dir, "agent-signin", ".credentials.json"), []byte(`{"fixture":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	login = stepByID(t, st, "login")
	if login["state"] != "unsupported" || login["action"] != "" {
		t.Fatal("legacy VM credential file re-enabled VM login")
	}

	for _, op := range []string{"t3-setup.login.open", "t3-setup.login.code", "t3-setup.run"} {
		if _, err := handle(t, l.h, "tok", op, map[string]any{"step": "login"}); err == nil || !strings.Contains(err.Error(), "retired") {
			t.Errorf("retired login %s: %v", op, err)
		}
	}
	// The agent setup being unavailable doesn't hide T3's facts.
	l.t3.AgentSetup = func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("no container tool")
	}
	st, err = handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil || st["running"] != true || !strings.Contains(st["agent_setup_error"].(string), "no container tool") {
		t.Errorf("status with the agent setup down: %v %v", st, err)
	}
}

func TestT3SetupRunInstallStartPair(t *testing.T) {
	l := newLife(t)
	release := make(chan struct{})
	inner := l.t3.Fetch
	l.t3.Fetch = func(ctx context.Context, url string, w io.Writer, max int64) error {
		<-release
		return inner(ctx, url, w, max)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("unknown step: %v", err)
	}
	res, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "install"})
	if err != nil || res["started"] != true {
		t.Fatalf("%v %v", res, err)
	}
	// The step runs in the background: the status shows it, a second step waits its turn.
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	if job, _ := st["t3_job"].(map[string]any); job == nil || job["step"] != "install" {
		t.Errorf("job: %v", st["t3_job"])
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "start"}); err == nil || !strings.Contains(err.Error(), "busy with the install step") {
		t.Errorf("second step: %v", err)
	}
	close(release)
	l.t3.Wait()
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if st["installed"] != true || st["t3_job"] != nil || st["t3_error"] != nil {
		t.Fatalf("after install: %v", st)
	}
	for _, step := range []string{"start", "pair"} {
		if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": step}); err != nil {
			t.Fatal(err)
		}
		l.t3.Wait()
	}
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if st["running"] != true || st["paired"] != true || len(l.pairs) != 2 {
		t.Errorf("after start and pair: %v (trades %v)", st, l.pairs)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "stop"}); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	if st, _ = handle(t, l.h, "tok", "t3-setup.status", nil); st["running"] != false || st["paired"] != false {
		t.Errorf("after stop: %v", st)
	}
}

// A failed step is reported on the step, in plain words, and clears when the next one starts.
func TestT3SetupFailureIsReported(t *testing.T) {
	l := newLife(t)
	l.kit.pin.Node.SHA256[testPlat] = sha256hex([]byte("other"))
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "install"}); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	e, _ := st["t3_error"].(map[string]any)
	if e == nil || e["step"] != "install" || !strings.Contains(e["message"].(string), "does not match") {
		t.Fatalf("error: %v", st["t3_error"])
	}
	var failed bool
	for _, s := range st["steps"].([]any) {
		if m := s.(map[string]any); m["id"] == "t3" && m["state"] == "failed" && strings.Contains(m["reason"].(string), "does not match") {
			failed = true
		}
	}
	if !failed {
		t.Errorf("the t3 step doesn't show the failure: %v", st["steps"])
	}
	// start without an install is a plain error on the start step
	l.t3.RunStep(T3StepStart)
	l.t3.Wait()
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if e, _ := st["t3_error"].(map[string]any); e == nil || e["step"] != "start" || !strings.Contains(e["message"].(string), "install") {
		t.Errorf("start error: %v", st["t3_error"])
	}
}

// t3-setup.* is also the agent setup: the container, image, project folders and sign-in go through unchanged.
func TestT3SetupPassesTheAgentSetupOn(t *testing.T) {
	l := newLife(t)
	var mu sync.Mutex
	var got []string
	l.t3.AgentSetup = func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, op+" "+string(args))
		return map[string]any{"ok": true}, nil
	}
	for _, c := range []struct{ op, args string }{
		{"run", `{"step":"projects"}`},
		{"project.add", `{}`}, {"project.remove", `{"dir":"/x"}`}, {"cancel", `{}`},
	} {
		if _, err := handle(t, l.h, "tok", "t3-setup."+c.op, json.RawMessage(c.args)); err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
	}
	want := []string{`agent-setup.run {"step":"projects"}`,
		`agent-setup.project.add {}`, `agent-setup.project.remove {"dir":"/x"}`, `agent-setup.cancel {}`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("passed on:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// "all" sets T3 up as well as the agent path.
	got = nil
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "all"}); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Errorf("all: %v", got)
	}
	if st := l.t3.Status(); st["installed"] != true || st["running"] != true || st["paired"] != true {
		t.Errorf("all left T3 as %v", st)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.nonsense", nil); err == nil {
		t.Error("an unknown t3-setup op was served")
	}
}

// Every t3 op needs the pairing like every other op, and hello says the capabilities are there.
func TestT3OpsNeedAPairedRubi(t *testing.T) {
	l := newLife(t)
	for _, op := range []string{"t3-agent.projects", "t3-agent.start", "t3-agent.next", "t3-agent.role.put", "t3-agent.ssh.register", "t3-setup.status", "t3-setup.run"} {
		if _, err := handle(t, l.h, "wrong", op, map[string]any{}); err == nil || !strings.Contains(err.Error(), "paired") {
			t.Errorf("%s unpaired: %v", op, err)
		}
	}
	hello, _ := handle(t, l.h, "tok", "hello", nil)
	have := map[string]bool{}
	for _, c := range hello["can"].([]any) {
		have[c.(string)] = true
	}
	if !have["t3-agent"] || !have["t3-setup"] || !have["claude-agent"] {
		t.Errorf("hello: %v", hello["can"])
	}
}

// A thread that was running when Home stopped is watched again at the next start.
func TestT3ResumesWatchingRunningThreads(t *testing.T) {
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	l.up(t)
	id := startThread(t, l, role, map[string]any{"prompt": "work"})
	l.t3.Stop()
	if got, _ := l.t3.threads.get(id); !got.Running {
		t.Fatal("the thread is not marked running")
	}
	// Home restarts: a new T3Home reads the thread list from the file.
	th2 := newT3Home(l.h)
	th2.Fetch, th2.Pin, th2.Platform, th2.NewServer, th2.Pair = l.t3.Fetch, l.t3.Pin, l.t3.Platform, l.t3.NewServer, l.t3.Pair
	th2.base = l.t3.base
	l.h.T3 = th2
	t.Cleanup(th2.Stop)
	if err := th2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.fake.Complete(id, "completed", "after the restart")
	items := nextItems(t, l.h, 2)
	if r := resultOf(t, items); r["text"] != "after the restart" {
		t.Errorf("result: %v", r)
	}
	st, _ := os.Stat(th2.threads.path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("thread list mode %v", st.Mode())
	}
}

func stepByID(t *testing.T, st map[string]any, id string) map[string]any {
	t.Helper()
	for _, s := range st["steps"].([]any) {
		if m := s.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no step %s", id)
	return nil
}

// The t3 row says install, then upgrade when what is installed is not the pin; the t3 step installs and starts.
func TestT3SetupT3StepAndActions(t *testing.T) {
	l := newLife(t)
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	if r := stepByID(t, st, "t3"); r["action"] != "install" || r["state"] != "todo" {
		t.Errorf("fresh: %v", r)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "t3"}); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if r := stepByID(t, st, "t3"); r["state"] != "done" || st["running"] != true || st["installed"] != true {
		t.Errorf("after the t3 step: %v", r)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "pair"}); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	l.t3.Stop()
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if r := stepByID(t, st, "t3"); r["state"] != "todo" || r["action"] != "install" || !strings.Contains(r["reason"].(string), "not running") {
		t.Errorf("stopped: %v", r)
	}
	l.kit.pin.Version = "9.9.10-test"
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if r := stepByID(t, st, "t3"); r["action"] != "upgrade" || !strings.Contains(r["reason"].(string), "pinned") {
		t.Errorf("pin moved: %v", r)
	}
}

func TestT3StartNeedsFreeMemory(t *testing.T) {
	l := newLife(t)
	l.install(t)
	l.t3.MemAvailable = func() (int, bool) { return t3MinFreeMiB - 1, true }
	err := l.t3.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "MiB of memory are free") {
		t.Fatalf("a start with too little memory: %v", err)
	}
	if l.srv.port != 0 || l.t3.srvAlive() {
		t.Fatal("the server was started anyway")
	}
	l.t3.MemAvailable = func() (int, bool) { return t3MinFreeMiB, true }
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatalf("a start with enough memory: %v", err)
	}
	// A running server isn't stopped or refused by a later low reading: the floor guards only a start.
	l.t3.MemAvailable = func() (int, bool) { return 10, true }
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatalf("Start on a running server: %v", err)
	}
}

// leftServer brings T3 up once (so a port is recorded), stops it, and has the next Adopt find a left server of ver.
func (l *life) leftServer(t *testing.T, ver string) {
	t.Helper()
	l.up(t)
	l.t3.Stop()
	l.srv.mu.Lock()
	l.srv.adoptOK, l.srv.adoptVer, l.srv.adopts, l.srv.started, l.srv.stopped = true, ver, 0, 0, 0
	l.srv.mu.Unlock()
}

func TestT3StartTakesOverALeftServer(t *testing.T) {
	l := newLife(t)
	l.leftServer(t, testVer)
	// A left server runs already: the memory floor, which guards a new start, doesn't apply.
	l.t3.MemAvailable = func() (int, bool) { return 10, true }
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.adopts != 1 || l.srv.started != 0 || l.srv.stopped != 0 {
		t.Errorf("adopts %d, starts %d, stops %d; want the left server taken over as it is", l.srv.adopts, l.srv.started, l.srv.stopped)
	}
	if !l.t3.srvAlive() || l.t3.client() == nil || l.t3.Pid() != 4242 {
		t.Error("the taken-over server isn't running and paired")
	}
	l.t3.Stop()
	if l.srv.stopped != 1 {
		t.Error("Stop leaves the taken-over server running")
	}
}

func TestT3StartStopsALeftServerOfAnotherVersion(t *testing.T) {
	l := newLife(t)
	l.leftServer(t, "0.0.1-old")
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.adopts != 1 || l.srv.stopped != 1 || l.srv.started != 1 {
		t.Errorf("adopts %d, stops %d, starts %d; want the old server stopped and a new one started", l.srv.adopts, l.srv.stopped, l.srv.started)
	}
}

func TestT3StartAdoptsNothingWithoutARecordedPort(t *testing.T) {
	l := newLife(t)
	l.install(t)
	l.srv.adoptOK, l.srv.adoptVer = true, testVer
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.adopts != 0 || l.srv.started != 1 {
		t.Errorf("adopts %d, starts %d before any port was recorded", l.srv.adopts, l.srv.started)
	}
}

func TestT3StartWhenAdoptFails(t *testing.T) {
	l := newLife(t)
	l.leftServer(t, testVer)
	l.srv.adoptErr = errors.New("boom")
	if err := l.t3.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.srv.started != 1 {
		t.Error("a failed check for a left server keeps T3 from starting")
	}
}

func TestT3StatusShowsMemoryProtection(t *testing.T) {
	l := newLife(t)
	l.h.hostProbe = func() (hostguard.Host, error) {
		return hostguard.Host{Yama: hostguard.Yama{Applicable: true, Present: true, Scope: 1}, Kernel: "k", MachineID: "m"}, nil
	}
	mp, ok := l.t3.Status()["memory_protection"].(MemoryProtectionState)
	if !ok || !mp.Applies || mp.OK || !strings.Contains(mp.Reason, "ptrace_scope is 1") {
		t.Errorf("memory_protection = %+v", mp)
	}
}
