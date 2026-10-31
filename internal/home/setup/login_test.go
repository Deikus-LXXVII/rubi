package setup

import (
	"context"
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
	if got := findURL("If the browser didn't open, visit: " + claudeURL); got != claudeURL {
		t.Errorf("findURL: %q", got)
	}
	if got := findURL("\x1b[1mvisit: " + claudeURL + ".\x1b[0m"); got != claudeURL {
		t.Errorf("findURL with escapes: %q", got)
	}
	if findURL("no link here") != "" {
		t.Error("found a link in nothing")
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

// scriptedLogin plays `claude auth login` without a terminal: the link, a prompt with no newline, then it
// waits for the code on stdin and ends.
func scriptedLogin(box *fakeBox, link string, gotCode chan string) func(c *fakeConn) {
	return func(c *fakeConn) {
		c.out.Write([]byte("If the browser didn't open, visit: " + link + "\n"))
		c.out.Write([]byte("Paste code here if prompted >"))
		for i := 0; i < 500; i++ {
			if s := c.stdinText(); strings.HasSuffix(s, "\n") {
				gotCode <- strings.TrimSpace(s)
				box.mu.Lock()
				box.loggedIn = true
				box.mu.Unlock()
				c.out.Write([]byte("\nLogin successful\n"))
				c.finish()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		c.finish()
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

func TestLoginFlow(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	got := make(chan string, 1)
	e.ag.box.login = scriptedLogin(e.ag.box, claudeURL, got)
	if _, err := e.op("run", map[string]any{"step": "login", "project": p.ID, "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link", func() bool {
		st := e.s.Status(context.Background())
		return st.Login != nil && st.Login.URL != "" && st.Login.WantsCode
	})
	st := e.s.Status(context.Background())
	if st.Login.Project != p.ID || st.Login.URL != claudeURL || st.Job.Step != "login" {
		t.Errorf("login: %+v job %+v", st.Login, st.Job)
	}
	// the link is shown as text, never in the progress lines
	for _, s := range st.Steps {
		for _, l := range s.Progress {
			if strings.Contains(l, "https://") || strings.Contains(l, "oauth") {
				t.Errorf("link in progress: %q", l)
			}
		}
		for _, r := range s.Projects {
			for _, l := range r.Progress {
				if strings.Contains(l, "https://") {
					t.Errorf("link in progress: %q", l)
				}
			}
		}
	}
	if x := e.ag.box.execs[0]; len(x) != 6 || x[0] != "timeout" || x[3] != "auth" || x[5] != "--claudeai" {
		t.Errorf("exec: %v", e.ag.box.execs)
	}
	// the code: validation, project match
	if _, err := e.op("login.code", map[string]any{"project": p.ID, "code": "has space"}); err == nil {
		t.Error("code with a space accepted")
	}
	if _, err := e.op("login.code", map[string]any{"project": "other", "code": "abc123"}); err == nil {
		t.Error("code for another project accepted")
	}
	if e.ag.box.lastLogin.stdinText() != "" {
		t.Error("something was written before a valid code")
	}
	res, err := e.op("login.code", map[string]any{"project": p.ID, "code": "AbC-123_xyz"})
	if err != nil || res["sent"] != true {
		t.Fatalf("code: %v %v", res, err)
	}
	if c := <-got; c != "AbC-123_xyz" {
		t.Errorf("stdin got %q", c)
	}
	e.wait()
	if st := e.step("login"); st.State != Done || st.Projects[0].Reason != "Signed in." {
		t.Errorf("after: %+v", st)
	}
	if _, err := e.op("login.code", map[string]any{"project": p.ID, "code": "abc"}); err == nil {
		t.Error("code accepted with no sign-in waiting")
	}
}

func TestLoginMethods(t *testing.T) {
	for method, flag := range map[string]string{"console": "--console", "claudeai": "--claudeai"} {
		e := newTestEnv(t)
		e.ready()
		p := e.addProject("my-project")
		e.ag.box.login = func(c *fakeConn) { c.finish() }
		e.op("run", map[string]any{"step": "login", "project": p.ID, "method": method})
		e.wait()
		found := false
		for _, x := range e.ag.box.execs {
			found = found || len(x) == 6 && x[0] == "timeout" && x[3] == "auth" && x[4] == "login" && x[5] == flag
		}
		if !found {
			t.Errorf("%s: %v", method, e.ag.box.execs)
		}
		// the process ended without a link: failed, not stuck
		if st := e.step("login"); st.State != Failed || !strings.Contains(st.Projects[0].Reason, "without showing a link") {
			t.Errorf("%s: %+v", method, st)
		}
	}
}

func TestLoginRefusesAnUnexpectedLink(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	e.ag.box.login = func(c *fakeConn) {
		c.out.Write([]byte("visit: https://claude.com.evil.example.com/oauth/authorize?code=true\n"))
		for i := 0; i < 100 && !c.isClosed(); i++ {
			time.Sleep(5 * time.Millisecond)
		}
		c.finish()
	}
	e.op("run", map[string]any{"step": "login", "project": p.ID, "method": "claudeai"})
	e.wait()
	st := e.step("login")
	if st.State != Failed || !strings.Contains(st.Projects[0].Reason, "doesn't point to a Claude or Anthropic") {
		t.Errorf("%+v", st)
	}
	if e.s.Status(context.Background()).Login != nil {
		t.Error("login info kept after the job")
	}
	// and it can't be opened
	if err := e.s.LoginOpen(context.Background(), p.ID); err == nil {
		t.Error("opened a link after the job")
	}
}

func TestLoginTimesOut(t *testing.T) {
	old := loginLimit
	loginLimit = 150 * time.Millisecond
	defer func() { loginLimit = old }()
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	e.ag.box.login = func(c *fakeConn) {
		c.out.Write([]byte("visit: " + claudeURL + "\n"))
		for !c.isClosed() {
			time.Sleep(5 * time.Millisecond)
		}
	}
	e.op("run", map[string]any{"step": "login", "project": p.ID, "method": "claudeai"})
	e.wait()
	if st := e.step("login"); st.State != Failed || !strings.Contains(st.Projects[0].Reason, "waited") {
		t.Errorf("%+v", st)
	}
}

// endedInVM says whether the cleanup of `claude auth login` was run in the VM.
func (b *fakeBox) endedInVM() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, x := range b.execs {
		if len(x) == 3 && x[0] == "node" && strings.Contains(x[2], "SIGTERM") && strings.Contains(x[2], "/proc") {
			return true
		}
	}
	return false
}

func TestLoginEndsInsideTheVMAfterTimeoutAndCancel(t *testing.T) {
	old := loginLimit
	loginLimit = 150 * time.Millisecond
	defer func() { loginLimit = old }()
	for _, how := range []string{"timeout", "cancel"} {
		e := newTestEnv(t)
		e.ready()
		p := e.addProject("my-project")
		e.ag.box.login = func(c *fakeConn) {
			c.out.Write([]byte("visit: " + claudeURL + "\n"))
			for !c.isClosed() {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if how == "cancel" {
			loginLimit = time.Minute
		}
		e.op("run", map[string]any{"step": "login", "project": p.ID, "method": "claudeai"})
		if how == "cancel" {
			waitFor(t, "the link", func() bool { st := e.s.Status(context.Background()); return st.Login != nil && st.Login.URL != "" })
			e.s.Cancel()
		}
		e.wait()
		if !e.ag.box.endedInVM() {
			t.Errorf("%s: no cleanup ran in the VM; execs %v", how, e.ag.box.execs)
		}
		// the backstop inside the VM outlives the Mac's own limit
		if x := e.ag.box.execs[0]; x[0] != "timeout" {
			t.Errorf("%s: %v", how, x)
		}
	}
	loginLimit = 150 * time.Millisecond
}

func TestLoginOpen(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	if _, err := e.op("login.open", map[string]any{"project": p.ID}); err == nil {
		t.Error("opened with no sign-in")
	}
	got := make(chan string, 1)
	e.ag.box.login = scriptedLogin(e.ag.box, claudeURL, got)
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if argv[0] == "/usr/bin/open" {
			return nil, nil, true
		}
		return nil, nil, false
	}
	e.op("run", map[string]any{"step": "login", "project": p.ID, "method": "claudeai"})
	waitFor(t, "the link", func() bool { st := e.s.Status(context.Background()); return st.Login != nil && st.Login.URL != "" })
	if _, err := e.op("login.open", map[string]any{"project": "other"}); err == nil {
		t.Error("opened the link of another project")
	}
	res, err := e.op("login.open", map[string]any{"project": p.ID})
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
	if err := e.s.LoginOpen(context.Background(), p.ID); err == nil {
		t.Error("opened on a non-Mac")
	}
}

func TestLoginSkipsWhenSignedInInAllAndChecksOnlyWithoutMethod(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	p := e.addProject("my-project")
	e.ag.box.login = func(c *fakeConn) { t.Error("login started"); c.finish() }
	// no method: check only, not signed in -> needs_user
	e.op("run", map[string]any{"step": "login", "project": p.ID})
	e.wait()
	if st := e.step("login"); st.State != NeedsUser {
		t.Errorf("%+v", st)
	}
	e.ag.box.loggedIn = true
	e.op("run", map[string]any{"step": "login", "project": p.ID})
	e.wait()
	if st := e.step("login"); st.State != Done {
		t.Errorf("%+v", st)
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
	e.ag.running[p.ID] = true
	e.ag.box.loggedIn = true
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
		e.ag.running[p.ID] = true
		e.ag.box.loggedIn = true
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
