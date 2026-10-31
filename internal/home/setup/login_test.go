package setup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

const (
	claudeURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000&response_type=code&state=abcdefghijklmnopqrstuvwxyz0123456789"
	goodProbe = `{"write":"ok","uid":10001,"dns":"EAI_AGAIN","direct_ip":"timeout","direct_default_gw":"ENETUNREACH","proxy_allowed":"HTTP/1.1 200 Connection Established","proxy_not_listed":"HTTP/1.1 403 Forbidden"}`
)

var goodTurn = agent.TurnResult{Result: "DENIED", Denied: []string{"Bash"}}

func TestLoginURLAllowList(t *testing.T) {
	for _, ok := range []string{claudeURL, "https://claude.ai/login", "https://platform.claude.com/oauth/authorize?x=1", "https://console.anthropic.com/oauth"} {
		if _, err := loginURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://claude.com/x", "https://evil.example.com/claude.com", "https://claude.com.evil.example.com/x", "https://evilclaude.com/x",
		"https://user@claude.com/x", "https://claude.com:8443/x", "https://claude.com@evil.example.com/x", "javascript:alert(1)",
		"https://anthropic.com/x", "https://sub.claude.com/x", "", "file:///etc/passwd", "https://claude.com\\@evil.example.com/",
	} {
		if _, err := loginURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestValidCode(t *testing.T) {
	long := strings.Repeat("a", 512)
	for _, ok := range []string{"abc", "ABC-123_xyz.~#abc", long} {
		if !ValidCode(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "a b", "abc\n", "abc\r", "tab\tx", "é", "a\x00b", long + "a", "a\x7fb", " "} {
		if ValidCode(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// scriptedHelper plays rubi-claude-token: the link, the prompt, then the code on stdin, then the token is
// stored (the fake never has a token's text) and SAVED.
func scriptedHelper(link string, gotCode chan string) func(c *fakeConn, a *fakeAuth) {
	return func(c *fakeConn, a *fakeAuth) {
		c.out.Write([]byte("URL " + link + "\n"))
		c.out.Write([]byte("CODE?\n"))
		for i := 0; i < 500; i++ {
			if s := c.stdinText(); strings.HasSuffix(s, "\n") {
				gotCode <- strings.TrimSpace(s)
				a.ag.mu.Lock()
				a.ag.token, a.ag.expires = true, time.Now().Add(365*24*time.Hour)
				a.ag.mu.Unlock()
				c.out.Write([]byte("SAVED\n"))
				c.finish()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		c.finish()
	}
}

// holdHelper prints what it is given, then waits until it is closed.
func holdHelper(out string) func(c *fakeConn, a *fakeAuth) {
	return func(c *fakeConn, a *fakeAuth) {
		c.out.Write([]byte(out))
		for !c.isClosed() {
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// readyForLogin is a Mac whose steps 1 to 4 are done, with one project.
func readyForLogin(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.ready()
	e.addProject("my-project")
	return e
}

func (e *testEnv) lastAuth() *fakeAuth {
	e.ag.mu.Lock()
	defer e.ag.mu.Unlock()
	if len(e.ag.sessions) == 0 {
		return nil
	}
	return e.ag.sessions[len(e.ag.sessions)-1]
}

func TestLoginFlow(t *testing.T) {
	e := readyForLogin(t)
	got := make(chan string, 1)
	e.ag.sess.login = scriptedHelper(claudeURL, got)
	if st := e.step("login"); st.State != Todo || st.Action != "login" || st.Reason != "Not signed in." || len(st.Projects) != 0 {
		t.Fatalf("before: %+v", st)
	}
	// no project is needed, and the one given is ignored
	if _, err := e.op("run", map[string]any{"step": "login", "method": "claudeai", "project": "whatever"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link", func() bool {
		st := e.s.Status(context.Background())
		return st.Login != nil && st.Login.URL != "" && st.Login.WantsCode
	})
	st := e.s.Status(context.Background())
	if st.Login.URL != claudeURL || st.Job.Step != "login" || st.Job.Project != "" {
		t.Errorf("login: %+v job %+v", st.Login, st.Job)
	}
	if row := st.Steps[5]; row.State != Working || row.Action != "cancel" {
		t.Errorf("row while it works: %+v", row)
	}
	// the link is shown as text, never in the progress lines
	raw, _ := json.Marshal(st.Steps)
	if strings.Contains(string(raw), "oauth") || strings.Contains(string(raw), "https://") {
		t.Errorf("link in a progress line: %s", raw)
	}
	a := e.lastAuth()
	if len(a.argv) != 1 || a.argv[0] != agent.AuthHelper || a.env["CLAUDE_CONFIG_DIR"] != "/tmp/claude-signin" {
		t.Errorf("exec: %v %v", a.argv, a.env)
	}
	// the code: validated, written only when valid
	for _, bad := range []string{"has space", "", "new\nline"} {
		if _, err := e.op("login.code", map[string]any{"code": bad}); err == nil {
			t.Errorf("code %q accepted", bad)
		}
	}
	if a.conn.stdinText() != "" {
		t.Error("something was written before a valid code")
	}
	res, err := e.op("login.code", map[string]any{"project": "ignored", "code": "AbC-123_xyz"})
	if err != nil || res["sent"] != true {
		t.Fatalf("code: %v %v", res, err)
	}
	if c := <-got; c != "AbC-123_xyz" {
		t.Errorf("stdin got %q", c)
	}
	e.wait()
	if !a.isClosed() {
		t.Error("the sign-in container wasn't ended")
	}
	e.s.forgetAuth()
	st2 := e.step("login")
	if st2.State != Done || !strings.Contains(st2.Reason, "Signed in until "+time.Now().Add(365*24*time.Hour).Format("2006-01-02")) || st2.Action != "login_again" || st2.Action2 != "sign_out" {
		t.Errorf("after: %+v", st2)
	}
	if _, err := e.op("login.code", map[string]any{"code": "abc"}); err == nil {
		t.Error("code accepted with no sign-in waiting")
	}
	// the project VMs are not touched by the sign-in
	if len(e.ag.box.execs) != 0 {
		t.Errorf("a project VM was used: %v", e.ag.box.execs)
	}
}

func TestLoginMethodsAndCheckOnly(t *testing.T) {
	e := readyForLogin(t)
	if _, err := e.op("run", map[string]any{"step": "login", "method": "console"}); err == nil || !strings.Contains(err.Error(), "claudeai") {
		t.Errorf("console: %v", err)
	}
	// without a method: only a check, no container
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) { t.Error("a sign-in started"); c.finish() }
	e.op("run", map[string]any{"step": "login"})
	e.wait()
	if st := e.step("login"); st.State != NeedsUser || len(e.ag.sessions) != 0 {
		t.Errorf("%+v", st)
	}
	e.ag.token, e.ag.expires = true, time.Now().Add(time.Hour)
	e.op("run", map[string]any{"step": "login"})
	e.wait()
	if st := e.step("login"); st.State != Done || len(e.ag.sessions) != 0 {
		t.Errorf("%+v", st)
	}
}

func TestLoginStates(t *testing.T) {
	e := readyForLogin(t)
	e.ag.token, e.ag.expires = true, time.Now().Add(-48*time.Hour)
	st := e.step("login")
	if st.State != Todo || !strings.Contains(st.Reason, "expired on") || st.Action != "login_again" || st.Action2 != "sign_out" {
		t.Errorf("expired: %+v", st)
	}
	e.s.forgetAuth()
	e.ag.expires = time.Time{}
	if st := e.step("login"); st.State != Done || st.Action != "login_again" {
		t.Errorf("unknown expiry: %+v", st)
	}
	e.s.forgetAuth()
	e.ag.statusErr = errors.New("the container tool is busy")
	if st := e.step("login"); st.State != Todo || !strings.Contains(st.Reason, "Couldn't check") || st.Action != "check" {
		t.Errorf("probe failed: %+v", st)
	}
	// a probe is reused for a minute, and not made while steps 1 to 4 aren't done
	e.s.forgetAuth()
	e.ag.statusErr = nil
	n := e.ag.statusN
	e.step("login")
	e.step("login")
	if e.ag.statusN != n+1 {
		t.Errorf("probes: %d", e.ag.statusN-n)
	}
	e.m.image = false
	e.s.forgetAuth()
	n = e.ag.statusN
	if st := e.step("login"); st.Reason != "Needs step 4 first" || e.ag.statusN != n {
		t.Errorf("login needs 4: %+v (probes %d)", st, e.ag.statusN-n)
	}
	// the self-test row waits for the sign-in, not for a per-project one
	e.m.image = true
	e.s.forgetAuth()
	e.ag.token, e.ag.expires = false, time.Time{}
	if st := e.step("selftest"); st.Projects[0].Reason != "Needs step 6 first" {
		t.Errorf("selftest without sign-in: %+v", st)
	}
	e.s.forgetAuth()
	e.ag.token = true
	if st := e.step("selftest"); st.Projects[0].Action != "selftest" || st.Projects[0].State != Todo {
		t.Errorf("selftest with sign-in: %+v", st)
	}
}

func TestLoginRefusesAnUnexpectedLink(t *testing.T) {
	e := readyForLogin(t)
	e.ag.sess.login = holdHelper("URL https://claude.com.evil.example.com/oauth/authorize?x=1\n")
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	st := e.step("login")
	if st.State != Failed || !strings.Contains(st.Reason, "doesn't point to a Claude or Anthropic") {
		t.Errorf("%+v", st)
	}
	if e.s.Status(context.Background()).Login != nil {
		t.Error("login info kept after the job")
	}
	if err := e.s.LoginOpen(context.Background()); err == nil {
		t.Error("opened a link after the job")
	}
	if !e.lastAuth().isClosed() {
		t.Error("container left running")
	}
}

func TestLoginHelperFailureAndMissingToken(t *testing.T) {
	e := readyForLogin(t)
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) {
		c.out.Write([]byte("FAILED the sign-in ended without finishing\n"))
		c.finish()
	}
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Reason, "ended without finishing") {
		t.Errorf("%+v", st)
	}
	// SAVED, but nothing is stored: not trusted
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) { c.out.Write([]byte("SAVED\n")); c.finish() }
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Reason, "no token is stored") {
		t.Errorf("%+v", st)
	}
	// the helper ends silently
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) { c.finish() }
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Reason, "without showing a link") {
		t.Errorf("%+v", st)
	}
	// the container can't start
	e.ag.startErr = errors.New("no network")
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Reason, "sign-in container") {
		t.Errorf("%+v", st)
	}
}

func TestLoginTimesOutAndCancelEndTheContainer(t *testing.T) {
	old := loginLimit
	loginLimit = 150 * time.Millisecond
	defer func() { loginLimit = old }()
	e := readyForLogin(t)
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\n")
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Reason, "waited") {
		t.Errorf("%+v", st)
	}
	if !e.lastAuth().isClosed() {
		t.Error("timeout: the container (with claude in it) wasn't ended")
	}
	loginLimit = time.Minute
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	waitFor(t, "the link", func() bool { st := e.s.Status(context.Background()); return st.Login != nil && st.Login.URL != "" })
	e.s.Cancel()
	e.wait()
	if !e.lastAuth().isClosed() {
		t.Error("cancel: the container wasn't ended")
	}
	if st := e.step("login"); st.State != Todo {
		t.Errorf("after cancel the probe decides: %+v", st)
	}
	loginLimit = 150 * time.Millisecond
}

func TestConcurrentSignInsAreSerialized(t *testing.T) {
	e := readyForLogin(t)
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\n")
	if _, err := e.op("run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"login", "signout", "image", "all"} {
		if _, err := e.op("run", map[string]any{"step": step, "method": "claudeai"}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
			t.Errorf("%s while a sign-in runs: %v", step, err)
		}
	}
	e.s.Cancel()
	e.wait()
	if len(e.ag.sessions) != 1 {
		t.Errorf("%d sign-in containers", len(e.ag.sessions))
	}
}

func TestSignOutRemovesItForEveryProject(t *testing.T) {
	e := readyForLogin(t)
	e.addProject("other-project")
	e.ag.token, e.ag.expires = true, time.Now().Add(time.Hour)
	if st := e.step("login"); st.State != Done {
		t.Fatal(st)
	}
	if _, err := e.op("run", map[string]any{"step": "signout"}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if e.ag.signOutN != 1 {
		t.Errorf("sign-out ran %d times", e.ag.signOutN)
	}
	e.s.forgetAuth()
	st := e.step("login")
	if st.State != Todo || st.Reason != "Not signed in." || st.Action != "login" {
		t.Errorf("after: %+v", st)
	}
	for _, p := range e.step("selftest").Projects {
		if p.Reason != "Needs step 6 first" {
			t.Errorf("%s: %+v", p.ID, p)
		}
	}
	// the job says what a sign-out does and doesn't do
	e.s.mu.Lock()
	e.s.last[key(StepLogin, "")] = result{}
	e.s.mu.Unlock()
	j, ctx, _ := e.s.begin(StepSignOut, agent.Project{})
	out := e.s.runSignOut(ctx, j)
	e.s.release(j)
	if out.State != Done || !strings.Contains(out.Reason, "claude.ai") {
		t.Errorf("%+v", out)
	}
	e.m.image = false
	if _, err := e.op("run", map[string]any{"step": "signout"}); err == nil || err.Error() != "needs step 4 first" {
		t.Errorf("signout needs the image: %v", err)
	}
}

func TestNothingOfTheSignInReachesStatusOrProgress(t *testing.T) {
	e := readyForLogin(t)
	// a helper that misbehaves: it prints token-like lines of every kind
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\nsk-ant-oat01-LEAKLEAKLEAKLEAKLEAKLEAK\nBearer abc\nFAILED code=zzzzzz sk-ant-oat01-LEAK2LEAK2LEAK2LEAK2\n")
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	waitFor(t, "the link", func() bool { st := e.s.Status(context.Background()); return st.Login != nil && st.Login.URL != "" })
	e.s.Cancel()
	e.wait()
	raw, _ := json.Marshal(e.s.Status(context.Background()))
	for _, bad := range []string{"LEAK", "Bearer", "code=", "sk-ant"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("%q in the status: %s", bad, raw)
		}
	}
}

func TestLoginOpen(t *testing.T) {
	e := readyForLogin(t)
	if _, err := e.op("login.open", nil); err == nil {
		t.Error("opened with no sign-in")
	}
	got := make(chan string, 1)
	e.ag.sess.login = scriptedHelper(claudeURL, got)
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if argv[0] == "/usr/bin/open" {
			return nil, nil, true
		}
		return nil, nil, false
	}
	e.op("run", map[string]any{"step": "login", "method": "claudeai"})
	waitFor(t, "the link", func() bool { st := e.s.Status(context.Background()); return st.Login != nil && st.Login.URL != "" })
	res, err := e.op("login.open", map[string]any{"project": "ignored"})
	if err != nil || res["opened"] != true {
		t.Fatalf("%v %v", res, err)
	}
	var open []string
	for _, c := range e.cmd.calls {
		if c[0] == "/usr/bin/open" {
			open = c
		}
	}
	if len(open) != 2 || open[1] != claudeURL {
		t.Errorf("open argv: %v", open)
	}
	e.s.Cancel()
	e.wait()
	e.s.opt.GOOS = "linux"
	if err := e.s.LoginOpen(context.Background()); err == nil {
		t.Error("opened on a non-Mac")
	}
}

func TestIsolationAndTurnChecks(t *testing.T) {
	if bad := checkIsolation([]byte(goodProbe+"\n"), true); bad != "" {
		t.Errorf("good probe: %s", bad)
	}
	mod := func(k, v string) []byte {
		return []byte(strings.Replace(goodProbe, k, v, 1))
	}
	for name, out := range map[string][]byte{
		"empty":        nil,
		"not json":     []byte("oops"),
		"not writable": mod(`"write":"ok"`, `"write":"EACCES"`),
		"wrong user":   mod(`"uid":10001`, `"uid":0`),
		"dns works":    mod(`"EAI_AGAIN"`, `"93.184.216.34"`),
		"direct ip":    mod(`"direct_ip":"timeout"`, `"direct_ip":"connected"`),
		"direct gw":    mod(`"direct_default_gw":"ENETUNREACH"`, `"direct_default_gw":"connected"`),
		"proxy blocks": mod(`HTTP/1.1 200 Connection Established`, `HTTP/1.1 403 Forbidden`),
		"proxy leaks":  mod(`"proxy_not_listed":"HTTP/1.1 403 Forbidden"`, `"proxy_not_listed":"HTTP/1.1 200 Connection Established"`),
	} {
		if checkIsolation(out, true) == "" {
			t.Errorf("%s passed", name)
		}
	}
	for _, ok := range []string{"ENETUNREACH", "EHOSTUNREACH", "ETIMEDOUT", "timeout"} {
		if bad := checkIsolation(mod(`"direct_ip":"timeout"`, `"direct_ip":"`+ok+`"`), true); bad != "" {
			t.Errorf("%s: %s", ok, bad)
		}
	}
	// without a known default network the gateway probe is skipped, but then it must say so
	if checkIsolation(mod(`"direct_default_gw":"ENETUNREACH"`, `"direct_default_gw":"skipped"`), false) != "" {
		t.Error("skipped gateway probe refused")
	}
	if checkIsolation(mod(`"direct_default_gw":"ENETUNREACH"`, `"direct_default_gw":"skipped"`), true) == "" {
		t.Error("a skipped probe passed although a gateway was given")
	}
	if checkTurn(goodTurn) != "" {
		t.Error("good turn failed")
	}
	for name, r := range map[string]agent.TurnResult{
		"error":      {IsError: true, Denied: []string{"Bash"}},
		"closed":     {Closed: true},
		"no ask":     {Result: "ok"},
		"wrong tool": {Result: "ok", Denied: []string{"Read"}},
	} {
		if checkTurn(r) == "" {
			t.Errorf("%s passed", name)
		}
	}
}

func TestSelfTestJob(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	e.ag.token = true
	e.ag.box.probeOut = goodProbe
	st := &fakeSelfTest{box: e.ag.box, turn: goodTurn}
	e.ag.selftest = st
	if _, err := e.op("run", map[string]any{"step": "selftest", "project": p.ID}); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if !st.ended {
		t.Error("the self-test wasn't ended")
	}
	if s := e.step("selftest"); s.State != Done || s.Projects[0].Action != "selftest" {
		t.Errorf("%+v", s)
	}
	// a failing check names itself, and a new run replaces the result
	e.ag.box.probeOut = strings.Replace(goodProbe, `"direct_ip":"timeout"`, `"direct_ip":"connected"`, 1)
	e.op("run", map[string]any{"step": "selftest", "project": p.ID})
	e.wait()
	s := e.step("selftest")
	if s.State != Failed || !strings.Contains(s.Reason, "connects to the Internet directly") {
		t.Errorf("%+v", s)
	}
	// a session in the way
	e.ag.selftest = nil
	e.op("run", map[string]any{"step": "selftest", "project": p.ID})
	e.wait()
	if s := e.step("selftest"); s.State != Failed || !strings.Contains(s.Reason, "running or waiting") {
		t.Errorf("%+v", s)
	}
}

func TestSelfTestProbesTheDefaultNetworkGatewayWhenKnown(t *testing.T) {
	for _, known := range []bool{true, false} {
		e := newTestEnv(t)
		e.ready()
		p := e.addProject("my-project")
		e.ag.token = true
		e.ag.selftest = &fakeSelfTest{box: e.ag.box, turn: goodTurn}
		probe := goodProbe
		if !known {
			probe = strings.Replace(goodProbe, `"direct_default_gw":"ENETUNREACH"`, `"direct_default_gw":"skipped"`, 1)
			e.m.extra = func(argv []string) ([]byte, error, bool) {
				if len(argv) > 3 && argv[1] == "network" && argv[3] == "default" {
					return nil, errors.New("no such network"), true
				}
				return nil, nil, false
			}
		} else {
			e.m.extra = func(argv []string) ([]byte, error, bool) {
				if len(argv) > 3 && argv[1] == "network" && argv[3] == "default" {
					return []byte(defaultNet(t)), nil, true
				}
				return nil, nil, false
			}
		}
		e.ag.box.probeOut = probe
		e.op("run", map[string]any{"step": "selftest", "project": p.ID})
		e.wait()
		if s := e.step("selftest"); s.State != Done {
			t.Errorf("known=%v: %+v", known, s)
		}
		var gw string
		for _, env := range e.ag.box.envs {
			if v, ok := env["RUBI_PROBE_GW"]; ok {
				gw = v
			}
		}
		if known != (gw != "") {
			t.Errorf("known=%v but the probe got gateway %q", known, gw)
		}
		if strings.Contains(isolationProbe, "192.168.64.1") {
			t.Error("a gateway address is hardcoded in the probe")
		}
	}
}
