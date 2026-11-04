package setup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const claudeURL = "https://claude.com/cai/oauth/authorize?code=true&client_id=00000000-0000-0000-0000-000000000000&response_type=code&state=abcdefghijklmnopqrstuvwxyz0123456789"

const (
	testHelper = "/usr/local/bin/rubi-claude-login"
	testSignIn = "/h/agent-signin"
)

func TestLoginURLAllowList(t *testing.T) {
	for _, ok := range []string{claudeURL, "https://claude.ai/login", "https://platform.claude.com/oauth/authorize?x=1", "https://console.anthropic.com/oauth"} {
		if _, err := loginURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://claude.com/x", "https://evil.example.com/claude.com", "https://claude.com.evil.example.com/x", "https://evilclaude.com/x",
		"https://user@claude.com/x", "https://claude.com:8443/x", "https://claude.com@evil.example.com/x", "javascript:alert(1)",
		"https://claude.com/x?state=sk-ant-oat01-ABCDEF", "https://claude.ai/SK-ANT-x", "https://anthropic.com/x", "https://sub.claude.com/x", "", "file:///etc/passwd", "https://claude.com\\@evil.example.com/",
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

// scriptedHelper plays rubi-claude-login: the link, the prompt, then the code on stdin, then claude's credential file is
// there (the fake never has its content) and SAVED.
func scriptedHelper(link string, gotCode chan string) func(c *fakeConn, a *fakeAuth) {
	return func(c *fakeConn, a *fakeAuth) {
		c.out.Write([]byte("URL " + link + "\n"))
		c.out.Write([]byte("CODE?\n"))
		for i := 0; i < 500; i++ {
			if s := c.stdinText(); strings.HasSuffix(s, "\n") {
				gotCode <- strings.TrimSpace(s)
				a.ag.mu.Lock()
				a.ag.token = true
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

// readyForLogin is a Mac whose steps 1 to 4 are done, with one project, and a Setup that can sign in: the helper and the
// environment of the T3 roles' sign-in.
func readyForLogin(t *testing.T) *testEnv {
	e := newTestEnv(t, func(o *Options) {
		o.LoginHelper = testHelper
		o.LoginEnv = func() map[string]string {
			return map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": testSignIn, "CLAUDE_CONFIG_DIR": "/tmp/claude-login"}
		}
	})
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

func (e *testEnv) linkShown() bool {
	st := e.s.Status(context.Background())
	return st.Login != nil && st.Login.URL != ""
}

func TestLoginFlow(t *testing.T) {
	e := readyForLogin(t)
	got := make(chan string, 1)
	e.ag.sess.login = scriptedHelper(claudeURL, got)
	if li := e.s.LoginProgress(); li.Working || li.State != "" {
		t.Fatalf("before: %+v", li)
	}
	if err := e.s.StartLogin("claudeai"); err != nil {
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
	if li := e.s.LoginProgress(); !li.Working || li.URL != claudeURL || !li.WantsCode {
		t.Errorf("progress: %+v", li)
	}
	// the link is shown as text, never in the progress lines
	raw, _ := json.Marshal(e.s.LoginProgress().Progress)
	if strings.Contains(string(raw), "oauth") || strings.Contains(string(raw), "https://") {
		t.Errorf("link in a progress line: %s", raw)
	}
	a := e.lastAuth()
	if len(a.argv) != 1 || a.argv[0] != testHelper || a.env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != testSignIn || a.env["CLAUDE_CONFIG_DIR"] != "/tmp/claude-login" {
		t.Errorf("exec: %v %v", a.argv, a.env)
	}
	// the code: validated, written only when valid
	for _, bad := range []string{"has space", "", "new\nline"} {
		if err := e.s.LoginCode(bad); err == nil {
			t.Errorf("code %q accepted", bad)
		}
	}
	if a.conn.stdinText() != "" {
		t.Error("something was written before a valid code")
	}
	if err := e.s.LoginCode("AbC-123_xyz"); err != nil {
		t.Fatal(err)
	}
	if c := <-got; c != "AbC-123_xyz" {
		t.Errorf("stdin got %q", c)
	}
	e.wait()
	if !a.isClosed() {
		t.Error("the sign-in container wasn't ended")
	}
	if li := e.s.LoginProgress(); li.Working || li.State != "" {
		t.Errorf("after a success: %+v", li)
	}
	if !e.ag.SignedIn(context.Background()) {
		t.Error("no credential file after the sign-in")
	}
	if err := e.s.LoginCode("abc"); err == nil {
		t.Error("code accepted with no sign-in waiting")
	}
}

func TestLoginNeedsAKnownMethodAndAHelper(t *testing.T) {
	e := readyForLogin(t)
	if err := e.s.StartLogin("console"); err == nil || !strings.Contains(err.Error(), "claudeai") {
		t.Errorf("console: %v", err)
	}
	bare := newTestEnv(t)
	if err := bare.s.StartLogin("claudeai"); err == nil || !strings.Contains(err.Error(), "no sign-in helper") {
		t.Errorf("without a helper: %v", err)
	}
	if len(e.ag.sessions)+len(bare.ag.sessions) != 0 {
		t.Error("a container started for a refused sign-in")
	}
}

// The agent setup itself no longer signs in, signs out or tests: those are the T3 Agents setup's.
func TestRunRefusesLoginSignOutAndSelfTest(t *testing.T) {
	e := readyForLogin(t)
	for _, step := range []string{"login", "signout", "selftest"} {
		if _, err := e.op("run", map[string]any{"step": step, "method": "claudeai"}); err == nil || !strings.Contains(err.Error(), "T3 Agents setup") {
			t.Errorf("%s: %v", step, err)
		}
	}
	if len(e.ag.sessions) != 0 || e.s.Busy() {
		t.Error("something started")
	}
	for _, row := range e.s.Status(context.Background()).Steps {
		if row.ID == "login" || row.ID == "selftest" {
			t.Errorf("the status still has a %s row", row.ID)
		}
	}
}

func TestLoginRefusesAnUnexpectedLink(t *testing.T) {
	for _, link := range []string{"https://claude.com.evil.example.com/oauth/authorize?x=1", "https://claude.ai/oauth?state=sk-ant-oat01-FAKEFAKEFAKE"} {
		e := readyForLogin(t)
		e.ag.sess.login = holdHelper("URL " + link + "\n")
		if err := e.s.StartLogin("claudeai"); err != nil {
			t.Fatal(err)
		}
		e.wait()
		li := e.s.LoginProgress()
		if li.State != Failed || !strings.Contains(li.Reason, "doesn't point to a Claude or Anthropic") {
			t.Errorf("%s: %+v", link, li)
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
}

func TestLoginHelperFailureAndMissingCredential(t *testing.T) {
	e := readyForLogin(t)
	run := func() Row {
		if err := e.s.StartLogin("claudeai"); err != nil {
			t.Fatal(err)
		}
		e.wait()
		li := e.s.LoginProgress()
		return Row{State: li.State, Reason: li.Reason}
	}
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) {
		c.out.Write([]byte("FAILED the sign-in ended without finishing\n"))
		c.finish()
	}
	if r := run(); r.State != Failed || !strings.Contains(r.Reason, "ended without finishing") {
		t.Errorf("%+v", r)
	}
	// SAVED, but no file is there: not trusted
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) { c.out.Write([]byte("SAVED\n")); c.finish() }
	if r := run(); r.State != Failed || !strings.Contains(r.Reason, "no credential file is there") {
		t.Errorf("%+v", r)
	}
	// the helper ends silently
	e.ag.sess.login = func(c *fakeConn, a *fakeAuth) { c.finish() }
	if r := run(); r.State != Failed || !strings.Contains(r.Reason, "without showing a link") {
		t.Errorf("%+v", r)
	}
	// the container can't start
	e.ag.startErr = errors.New("no network")
	if r := run(); r.State != Failed || !strings.Contains(r.Reason, "sign-in container") {
		t.Errorf("%+v", r)
	}
}

func TestLoginTimesOutAndCancelEndTheContainer(t *testing.T) {
	old := loginLimit
	loginLimit = 150 * time.Millisecond
	defer func() { loginLimit = old }()
	e := readyForLogin(t)
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\n")
	if err := e.s.StartLogin("claudeai"); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if li := e.s.LoginProgress(); li.State != Failed || !strings.Contains(li.Reason, "waited") {
		t.Errorf("%+v", li)
	}
	if !e.lastAuth().isClosed() {
		t.Error("timeout: the container (with claude in it) wasn't ended")
	}
	loginLimit = time.Minute
	if err := e.s.StartLogin("claudeai"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link", e.linkShown)
	e.s.Cancel()
	e.wait()
	if !e.lastAuth().isClosed() {
		t.Error("cancel: the container wasn't ended")
	}
	if li := e.s.LoginProgress(); li.Working || li.State == Failed {
		t.Errorf("after cancel: %+v", li)
	}
	loginLimit = 150 * time.Millisecond
}

func TestConcurrentSignInsAreSerialized(t *testing.T) {
	e := readyForLogin(t)
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\n")
	if err := e.s.StartLogin("claudeai"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.StartLogin("claudeai"); err == nil || !strings.HasPrefix(err.Error(), "busy") {
		t.Errorf("a second sign-in: %v", err)
	}
	for _, step := range []string{"image", "all"} {
		if _, err := e.op("run", map[string]any{"step": step}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
			t.Errorf("%s while a sign-in runs: %v", step, err)
		}
	}
	e.s.Cancel()
	e.wait()
	if len(e.ag.sessions) != 1 {
		t.Errorf("%d sign-in containers", len(e.ag.sessions))
	}
}

func TestNothingOfTheSignInReachesStatusOrProgress(t *testing.T) {
	e := readyForLogin(t)
	// a helper that misbehaves: it prints token-like lines of every kind
	e.ag.sess.login = holdHelper("URL " + claudeURL + "\nsk-ant-oat01-LEAKLEAKLEAKLEAKLEAKLEAK\nBearer abc\nFAILED code=zzzzzz sk-ant-oat01-LEAK2LEAK2LEAK2LEAK2\n")
	if err := e.s.StartLogin("claudeai"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link", e.linkShown)
	e.s.Cancel()
	e.wait()
	raw, _ := json.Marshal([]any{e.s.Status(context.Background()), e.s.LoginProgress()})
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
	op := &fakeOpener{}
	e.s.opt.Opener = op
	if err := e.s.StartLogin("claudeai"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the link", e.linkShown)
	res, err := e.op("login.open", map[string]any{"project": "ignored"})
	if err != nil || res["opened"] != true {
		t.Fatalf("%v %v", res, err)
	}
	if len(op.links) != 1 || op.links[0] != claudeURL {
		t.Errorf("opened: %v", op.links)
	}
	for _, c := range e.cmd.calls {
		if c[0] == "/usr/bin/open" {
			t.Errorf("a command was run to open the link: %v", c)
		}
	}
	e.s.Cancel()
	e.wait()
	e.s.opt.GOOS = "linux"
	if err := e.s.LoginOpen(context.Background()); err == nil {
		t.Error("opened on a non-Mac")
	}
}

// fakeOpener is the browser: it records the links it was asked to open.
type fakeOpener struct {
	links []string
	err   error
}

func (f *fakeOpener) Open(_ context.Context, link string) error {
	f.links = append(f.links, link)
	return f.err
}
