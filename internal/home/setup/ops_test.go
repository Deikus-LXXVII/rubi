package setup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

func (e *testEnv) op(op string, args any) (map[string]any, error) {
	e.t.Helper()
	b, _ := json.Marshal(args)
	v, err := e.s.Handle(context.Background(), "agent-setup."+op, b)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func TestRunArguments(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.addProject("my-project")
	for name, args := range map[string]map[string]any{
		"unknown step":  {"step": "bogus"},
		"no step":       {},
		"bad method":    {"step": "login", "project": "my-project", "method": "password"},
		"unknown proj":  {"step": "selftest", "project": "nope"},
		"console":       {"step": "login", "method": "console"},
		"bad option":    {"step": "network", "option": "rm-rf"},
		"option, step":  {"step": "image", "option": "auto-address"},
		"projects step": {"step": "projects"},
	} {
		if _, err := e.op("run", args); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := e.s.Handle(context.Background(), "agent-setup.run", json.RawMessage(`{"step":`)); err == nil || err.Error() != "bad arguments" {
		t.Errorf("bad json: %v", err)
	}
	if _, err := e.s.Handle(context.Background(), "agent-setup.nope", nil); err == nil {
		t.Error("unknown op")
	}
	if e.s.Busy() {
		t.Error("a refused run left a job")
	}
}

func TestRunNeedsEarlierSteps(t *testing.T) {
	e := newTestEnv(t)
	for step, want := range map[string]string{
		"service": "needs step 1 first", "network": "needs step 1 first", "image": "needs step 1 first",
	} {
		if _, err := e.op("run", map[string]any{"step": step}); err == nil || err.Error() != want {
			t.Errorf("%s: %v", step, err)
		}
	}
	e.ready()
	e.m.network = ""
	if _, err := e.op("run", map[string]any{"step": "image"}); err == nil || err.Error() != "needs step 3 first" {
		t.Errorf("image: %v", err)
	}
	e.addProject("my-project")
	if _, err := e.op("run", map[string]any{"step": "login"}); err == nil || err.Error() != "needs step 3 first" {
		t.Errorf("login: %v", err)
	}
	e.m.network = internalNet(t)
	if _, err := e.op("run", map[string]any{"step": "selftest", "project": "my-project"}); err == nil || err.Error() != "needs step 6 first" {
		t.Errorf("selftest: %v", err)
	}
	if _, err := e.op("run", map[string]any{"step": "login"}); err != nil {
		t.Errorf("login with the first project: %v", err)
	}
	e.wait()
}

func TestBusyAndCancel(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.image = false
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var once sync.Once
	prev := e.m.extra
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 1 && argv[1] == "build" {
			once.Do(func() { started <- struct{}{} })
			<-release
			return nil, context.Canceled, true
		}
		if prev != nil {
			return prev(argv)
		}
		return nil, nil, false
	}
	res, err := e.op("run", map[string]any{"step": "image"})
	if err != nil || res["started"] != true {
		t.Fatalf("run: %v %v", res, err)
	}
	<-started
	if _, err := e.op("run", map[string]any{"step": "image"}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
		t.Errorf("second run: %v", err)
	}
	if _, err := e.op("run", map[string]any{"step": "all"}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
		t.Errorf("all while busy: %v", err)
	}
	st := e.s.Status(context.Background())
	if st.Job == nil || st.Job.Step != "image" || st.Job.Started.IsZero() {
		t.Errorf("job: %+v", st.Job)
	}
	img := st.Steps[3]
	if img.State != Working || img.Action != "cancel" {
		t.Errorf("image row: %+v", img)
	}
	if r, _ := e.op("cancel", nil); r["stopped"] != true {
		t.Errorf("cancel: %v", r)
	}
	close(release)
	e.wait()
	if st := e.step("image"); st.State != Todo {
		t.Errorf("after cancel the probe decides: %+v", st)
	}
	if r, _ := e.op("cancel", nil); r["stopped"] != false {
		t.Errorf("cancel with nothing running: %v", r)
	}
}

func TestServiceJob(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.sysJSON = ""
	prev := e.m.extra
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 2 && argv[1] == "system" && argv[2] == "start" {
			e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", e.appDir+"/")
			return []byte("ok"), nil, true
		}
		return prev(argv)
	}
	if _, err := e.op("run", map[string]any{"step": "service"}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if st := e.step("service"); st.State != Done {
		t.Errorf("%+v", st)
	}
	if got := e.cmd.ran("container", "system", "start", "--enable-kernel-install"); got != 1 {
		t.Errorf("start ran %d times", got)
	}
	// running already: nothing is started
	e.op("run", map[string]any{"step": "service"})
	e.wait()
	if e.cmd.ran("container", "system", "start") != 1 {
		t.Error("started a running service")
	}
}

func TestServiceJobTimesOut(t *testing.T) {
	old := serviceWait
	serviceWait = 200 * time.Millisecond
	defer func() { serviceWait = old }()
	e := newTestEnv(t)
	e.ready()
	e.m.sysJSON = ""
	e.op("run", map[string]any{"step": "service"})
	e.wait()
	st := e.step("service")
	if st.State != Failed || !strings.Contains(st.Reason, "didn't start") {
		t.Errorf("%+v", st)
	}
}

func TestNetworkJob(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.network = ""
	prev := e.m.extra
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 2 && argv[1] == "network" && argv[2] == "create" {
			e.m.network = internalNet(t)
			return nil, nil, true
		}
		return prev(argv)
	}
	e.cfg.ProxyListen = "127.0.0.1:3128"
	e.op("run", map[string]any{"step": "network"})
	e.wait()
	// the network exists and is host-only, but the proxy address is the user's call: not cleared on its own
	if st := e.step("network"); st.State != NeedsUser || e.cfg.ProxyListen == "" {
		t.Errorf("%+v %q", st, e.cfg.ProxyListen)
	}
	if e.cmd.ran("container", "network", "create", "--internal", "rubi-agent") != 1 {
		t.Errorf("create: %v", e.cmd.calls)
	}
	e.op("run", map[string]any{"step": "network", "option": "auto-address"})
	e.wait()
	if st := e.step("network"); st.State != Done || e.cfg.ProxyListen != "" || e.cfg.ProxyVMHost != "" {
		t.Errorf("%+v %+v", st, e.cfg)
	}
	if e.cmd.ran("container", "network", "create") != 1 {
		t.Error("created an existing network again")
	}
}

func TestNetworkJobNeverTouchesAnotherNetwork(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.cfg.Network = "default"
	e.m.network = defaultNet(t)
	e.op("run", map[string]any{"step": "network"})
	e.wait()
	st := e.step("network")
	if st.State != Failed || e.cmd.ran("container", "network", "create") != 0 || e.cmd.ran("container", "network", "delete") != 0 {
		t.Errorf("%+v %v", st, e.cmd.calls)
	}
}

func TestImageJob(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.image = false
	e.op("run", map[string]any{"step": "image"})
	e.wait()
	if !e.m.built || e.step("image").State != Done {
		t.Fatalf("built %v: %+v", e.m.built, e.step("image"))
	}
	var build []string
	for _, c := range e.cmd.calls {
		if len(c) > 1 && c[1] == "build" {
			build = c
		}
	}
	dir := build[len(build)-1]
	if filepath.Dir(dir) != filepath.Join(e.dir, "runner-build") || build[0] != "container" || build[len(build)-2] != filepath.Join(dir, "container", "Dockerfile") {
		t.Errorf("build: %v", build)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "main.ts")); err != nil {
		t.Errorf("the bundled runner wasn't written: %v", err)
	}
	e.op("run", map[string]any{"step": "image"})
	e.wait()
	if e.cmd.ran("container", "build") != 1 {
		t.Error("rebuilt an existing image")
	}
}

func TestImageJobFailureShowsTheLastLinesWithoutSecrets(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.image = false
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 1 && argv[1] == "build" {
			return []byte("#5 RUN npm ci\nnpm error network request failed\nAuthorization: Bearer abc123\nERROR: failed to solve\n"), errors.New("exit status 1"), true
		}
		return nil, nil, false
	}
	e.op("run", map[string]any{"step": "image"})
	e.wait()
	st := e.step("image")
	if st.State != Failed || !strings.Contains(st.Reason, "failed to solve") || strings.Contains(st.Reason+strings.Join(st.Progress, ""), "Bearer") {
		t.Errorf("%+v", st)
	}
	if len(st.Progress) == 0 {
		t.Error("no progress kept")
	}
}

func TestRunAllStopsAtTheFirstStepThatNeedsTheUser(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	// no project: steps 1 to 4 are done, step 5 needs the user
	if _, err := e.op("run", map[string]any{"step": "all"}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	st := e.step("projects")
	if st.State != NeedsUser || !strings.Contains(st.Reason, "Add a project folder") {
		t.Errorf("%+v", st)
	}
}

// allEnv is a Mac with the container tool installed and the network made, a stopped service, no image, and
// a signed-in project: what "all" is allowed to finish.
func allEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.ready()
	e.m.image = false
	e.m.sysJSON = ""
	p := e.addProject("my-project")
	e.ag.token, e.ag.expires = true, time.Now().Add(time.Hour) // signed in already: the sign-in step is skipped
	_ = p
	e.ag.box.probeOut = goodProbe
	e.ag.selftest = &fakeSelfTest{box: e.ag.box, turn: goodTurn}
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 2 && argv[1] == "system" && argv[2] == "start" {
			e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", e.appDir+"/")
			return nil, nil, true
		}
		return nil, nil, false
	}
	return e
}

// noPrivilegedCommand fails the test if "all" ran the installer, its downloads or the network creation.
func noPrivilegedCommand(t *testing.T, e *testEnv) {
	t.Helper()
	for _, c := range e.cmd.calls {
		j := strings.Join(c, " ")
		if c[0] == "/usr/bin/osascript" || c[0] == "/usr/sbin/pkgutil" || strings.HasPrefix(j, "container network create") {
			t.Errorf("a privileged command ran: %s", j)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "setup")); err == nil {
		t.Error("something was downloaded")
	}
}

func TestRunAllWholeWay(t *testing.T) {
	e := allEnv(t)
	if _, err := e.op("run", map[string]any{"step": "all"}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	for _, id := range []string{"container", "service", "network", "image", "projects", "login", "selftest"} {
		if st := e.step(id); st.State != Done {
			t.Errorf("%s: %+v", id, st)
		}
	}
	noPrivilegedCommand(t, e)
}

func TestRunAllNeverRunsThePrivilegedSteps(t *testing.T) {
	for name, c := range map[string]struct {
		break_ func(e *testEnv)
		step   string
		n      string
	}{
		"container missing": {func(e *testEnv) { e.m.version = "" }, "container", "1"},
		"container older":   {func(e *testEnv) { e.m.version = "container CLI version 1.0.0 (build: release, commit: abc)\n" }, "container", "1"},
		"network missing":   {func(e *testEnv) { e.m.network = "" }, "network", "3"},
	} {
		e := allEnv(t)
		c.break_(e)
		if _, err := e.op("run", map[string]any{"step": "all"}); err != nil {
			t.Fatal(err)
		}
		e.wait()
		st := e.step(c.step)
		if st.State != NeedsUser || !strings.Contains(st.Reason, "needs step "+c.n+" (asks for approval)") {
			t.Errorf("%s: %+v", name, st)
		}
		noPrivilegedCommand(t, e)
		if e.cmd.ran("container", "build") != 0 && c.step == "container" {
			t.Errorf("%s: went on after the stop", name)
		}
	}
}

// The tool or the network goes away between the plugin's check and Home's run, or while "all" runs.
func TestRunAllStopsWhenAPrivilegedStepRegressesMidRun(t *testing.T) {
	e := allEnv(t)
	prev := e.m.extra
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 1 && argv[1] == "build" { // the network disappears while the image builds
			e.m.image = true
			e.m.network = ""
			return []byte("built"), nil, true
		}
		return prev(argv)
	}
	e.op("run", map[string]any{"step": "all"})
	e.wait()
	if st := e.step("network"); st.State != NeedsUser || !strings.Contains(st.Reason, "needs step 3 (asks for approval)") {
		t.Errorf("network: %+v", st)
	}
	if len(e.ag.box.execs) != 0 {
		t.Errorf("the sign-in or the self-test started: %v", e.ag.box.execs)
	}
	noPrivilegedCommand(t, e)

	e = allEnv(t)
	prev = e.m.extra
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 2 && argv[1] == "system" && argv[2] == "start" { // the tool is removed as the service starts
			e.m.version = ""
			e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", e.appDir+"/")
			return nil, nil, true
		}
		return prev(argv)
	}
	e.op("run", map[string]any{"step": "all"})
	e.wait()
	if st := e.step("container"); st.State != NeedsUser || !strings.Contains(st.Reason, "needs step 1 (asks for approval)") {
		t.Errorf("container: %+v", st)
	}
	if e.cmd.ran("container", "build") != 0 {
		t.Error("the image was built after the tool went away")
	}
	noPrivilegedCommand(t, e)
}

func TestAutoAddressNeverCreatesTheNetwork(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.network = ""
	e.cfg.ProxyListen, e.cfg.ProxyVMHost = "127.0.0.1:3128", "host.example.com"
	// refused right away, and if it got through, the job would still not create anything
	if _, err := e.op("run", map[string]any{"step": "network", "option": "auto-address"}); err == nil || !strings.Contains(err.Error(), "its own approval") {
		t.Fatalf("run: %v", err)
	}
	j, ctx, err := e.s.begin(StepNetwork, agentProject())
	if err != nil {
		t.Fatal(err)
	}
	out := e.s.runNetwork(ctx, j, true)
	e.s.release(j)
	if out.State != Failed || !strings.Contains(out.Reason, "needs its own approval") {
		t.Errorf("job: %+v", out)
	}
	if e.cmd.ran("container", "network", "create") != 0 || e.cfg.ProxyListen == "" || e.cfg.ProxyVMHost == "" {
		t.Errorf("something changed: %v %+v", e.cmd.calls, e.cfg)
	}
	// an existing host-only network: the addresses are cleared
	e.m.network = internalNet(t)
	if _, err := e.op("run", map[string]any{"step": "network", "option": "auto-address"}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if e.cfg.ProxyListen != "" || e.cfg.ProxyVMHost != "" || e.step("network").State != Done || e.cmd.ran("container", "network", "create") != 0 {
		t.Errorf("%+v", e.cfg)
	}
	// a network that isn't host-only: nothing is cleared
	e.cfg.Network, e.cfg.ProxyListen = "default", "127.0.0.1:3128"
	e.m.network = defaultNet(t)
	e.op("run", map[string]any{"step": "network", "option": "auto-address"})
	e.wait()
	if e.cfg.ProxyListen == "" || e.step("network").State != Failed {
		t.Errorf("%+v %+v", e.cfg, e.step("network"))
	}
}

func TestStatusJSONShape(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.addProject("my-project")
	res, err := e.op("status", nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := res["steps"].([]any)
	if len(steps) != 7 {
		t.Fatalf("%v", res)
	}
	first := steps[0].(map[string]any)
	for _, k := range []string{"id", "title", "state", "reason"} {
		if _, ok := first[k]; !ok {
			t.Errorf("missing %s: %v", k, first)
		}
	}
	if _, ok := first["progress"]; ok {
		t.Error("empty progress is serialized")
	}
	if _, ok := res["job"]; ok {
		t.Error("job present while idle")
	}
	if _, ok := res["login"]; ok {
		t.Error("login present while idle")
	}
	login := steps[5].(map[string]any)
	if _, ok := login["projects"]; ok || login["action"] != "check" && login["action"] != "login" {
		t.Errorf("login row: %v", login)
	}
	row := steps[6].(map[string]any)["projects"].([]any)[0].(map[string]any)
	if row["id"] != "my-project" || row["dir"] == "" || row["state"] != "todo" || row["reason"] != "Needs step 6 first" {
		t.Errorf("row: %v", row)
	}
}

func TestFilter(t *testing.T) {
	for _, bad := range []string{
		"Authorization: Bearer abc", "x-api-key sk-ant-api03-xyz", "refresh_token=zzz", "Refresh the cache", "GET /cb?code=abc&state=1",
		"my TOKEN is here", strings.Repeat("A1b2", 12), "key aGVsbG8gd29ybGQgdGhpcyBpcyBsb25nIGVub3VnaCB0byBiZSBhIGtleQ==",
		"https://claude.com/cai/oauth/authorize?code=true&client_id=x&state=y", "   ", "",
		"paste: AbCdEfGh.IjKlMnOp_QrStUvWx-YzAbCdEf#1234567", "see abcdefgh~ijklmnop+qrstuvwx/yz0123456789==",
	} {
		if got, ok := clean(bad); ok {
			t.Errorf("%q kept as %q", bad, got)
		}
	}
	for in, want := range map[string]string{
		"#5 [builder 2/4] RUN npm ci --ignore-scripts": "#5 [builder 2/4] RUN npm ci --ignore-scripts",
		"tab\tand\x00nul":         "tab and nul",
		"Downloaded 31 of 148 MB": "Downloaded 31 of 148 MB",
	} {
		if got, ok := clean(in); !ok || got != want {
			t.Errorf("%q -> %q %v", in, got, ok)
		}
	}
	long, _ := clean(strings.Repeat("ab ", 200))
	if n := len([]rune(long)); n > 200 {
		t.Errorf("%d characters", n)
	}
	var r ring
	for i := 0; i < 50; i++ {
		r.add("line " + string(rune('a'+i%26)))
		r.add("Bearer secret")
	}
	if len(r.lines) != 20 {
		t.Errorf("ring holds %d lines", len(r.lines))
	}
	for _, l := range r.lines {
		if strings.Contains(l, "Bearer") {
			t.Error("secret line kept")
		}
	}
	if got := safeText("ok one\nBearer x\nok two"); got != "ok one; ok two" {
		t.Errorf("safeText: %q", got)
	}
}

func TestProgressNeverCarriesSecretsEvenFromCommands(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	e.m.image = false
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if len(argv) > 1 && argv[1] == "build" {
			e.m.image = true
			return []byte("step 1\nnpm token sk-ant-oat01-abcdef\nstep 2\n"), nil, true
		}
		return nil, nil, false
	}
	e.s.opt.Release = Pinned
	e.op("run", map[string]any{"step": "image"})
	e.wait()
	raw, _ := json.Marshal(e.s.Status(context.Background()))
	if strings.Contains(string(raw), "sk-ant") || strings.Contains(string(raw), "token") {
		t.Errorf("status carries a secret: %s", raw)
	}
}

func agentProject() agent.Project { return agent.Project{} }
