package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

// loginConn plays the sign-in helper's side of the line protocol: the lines it prints, and the lines it is sent.
type loginConn struct {
	out   *io.PipeWriter
	r     *io.PipeReader
	inR   *io.PipeReader
	inW   *io.PipeWriter
	done  chan struct{}
	once  sync.Once
	lines chan string // what Home sent to the helper's stdin
}

func newLoginConn() *loginConn {
	r, w := io.Pipe()
	ir, iw := io.Pipe()
	c := &loginConn{out: w, r: r, inR: ir, inW: iw, done: make(chan struct{}), lines: make(chan string, 16)}
	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, err := ir.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexByte(acc, '\n')
				if i < 0 {
					break
				}
				c.lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}
func (c *loginConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *loginConn) Write(p []byte) (int, error) { return c.inW.Write(p) }
func (c *loginConn) CloseWrite() error           { return nil }
func (c *loginConn) Close() error                { c.finish(); return nil }
func (c *loginConn) Done() <-chan struct{}       { return c.done }
func (c *loginConn) finish() {
	c.once.Do(func() { close(c.done); c.out.Close(); c.inW.Close() })
}
func (c *loginConn) say(l string) { _, _ = io.WriteString(c.out, l+"\n") }

// loginBox is the sign-in container: it records what was started and how, and plays the helper with script.
type loginBox struct {
	t        *testing.T
	dir      string
	mu       sync.Mutex
	starts   int
	closed   int
	startErr error
	argv     []string
	env      map[string]string
	conn     *loginConn
	script   func(b *loginBox, c *loginConn)
}

func (b *loginBox) Projects() []agent.Project     { return nil }
func (b *loginBox) SignedIn(context.Context) bool { return agent.SignedInAt(b.dir) }
func (b *loginBox) StartAuth(context.Context) (setup.AuthSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	if b.startErr != nil {
		return nil, b.startErr
	}
	return loginSession{b}, nil
}

type loginSession struct{ b *loginBox }

func (s loginSession) Exec(_ context.Context, argv []string, env map[string]string) (agent.Conn, error) {
	b := s.b
	c := newLoginConn()
	b.mu.Lock()
	b.argv, b.env, b.conn = argv, env, c
	script := b.script
	b.mu.Unlock()
	go script(b, c)
	return c, nil
}
func (s loginSession) Close() { s.b.mu.Lock(); s.b.closed++; s.b.mu.Unlock() }

const loginLink = "https://claude.com/cai/oauth/authorize?code=true&state=abc"

// goodScript prints the link and the prompt, and on the code it expects writes the credential (unreadable: Home must
// only stat it) and says SAVED.
func goodScript(code string) func(b *loginBox, c *loginConn) {
	return func(b *loginBox, c *loginConn) {
		c.say("URL " + loginLink)
		c.say("CODE?")
		for {
			select {
			case l := <-c.lines:
				if l != code {
					c.say("CODE?")
					continue
				}
				_ = os.MkdirAll(b.dir, 0o700)
				_ = os.WriteFile(filepath.Join(b.dir, agent.CredentialFile), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-FAKE"}}`), 0o000)
				c.say("SAVED")
				c.finish()
				return
			case <-c.done:
				return
			}
		}
	}
}

type fakeOpen struct {
	mu     sync.Mutex
	runs   [][]string
	opened []string // the links the Opener was asked to open
}

func (f *fakeOpen) Open(_ context.Context, link string) error {
	f.mu.Lock()
	f.opened = append(f.opened, link)
	f.mu.Unlock()
	return nil
}

func (f *fakeOpen) Run(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.runs = append(f.runs, argv)
	f.mu.Unlock()
	return nil, nil
}
func (f *fakeOpen) Stream(context.Context, []string, func(string)) error { return nil }

func loginLife(t *testing.T) (*life, *loginBox, *fakeOpen, string) {
	t.Helper()
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	box := &loginBox{t: t, dir: agent.SignInDirOf(l.h.Dir), script: goodScript("CODE-9876-xyz")}
	op := &fakeOpen{}
	l.t3.LoginAgents = box
	l.t3.LoginOptions = func(o *setup.Options) {
		o.Cmd, o.Opener = op, op
		o.Backend = func() agent.VMBackend { return agent.NewAppleBackend(agent.BackendConfig{}) } // the sign-in runs as on a Mac
	}
	t.Cleanup(func() { l.t3.login().Cancel() })
	return l, box, op, role
}

func loginBlock(t *testing.T, l *life) (step map[string]any, top any) {
	t.Helper()
	st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	return stepByID(t, st, "login"), st["login"]
}

func waitLogin(t *testing.T, l *life, ok func(step map[string]any) bool) map[string]any {
	t.Helper()
	var step map[string]any
	waitUntil(t, func() bool { step, _ = loginBlock(t, l); return ok(step) })
	return step
}

func TestT3LoginShowsTheLinkThenTakesTheCodeAndIsSharedByEveryRole(t *testing.T) {
	l, box, _, role := loginLife(t)
	second := putRole(t, l.h, "builder")
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai", "role": role}); err != nil {
		t.Fatal(err)
	}
	// the link and "wants the code" show on the step, on every role's row and on the top-level login block
	step := waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	if step["url"] != loginLink || step["state"] != "working" || step["action"] != "cancel" {
		t.Errorf("step: %v", step)
	}
	rows := step["projects"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows: %v", rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		m := r.(map[string]any)
		seen[m["id"].(string)] = true
		if m["url"] != loginLink || m["wants_code"] != true || m["state"] != "working" {
			t.Errorf("row: %v", m)
		}
	}
	if !seen[role] || !seen[second] {
		t.Errorf("rows are not the roles: %v", seen)
	}
	_, top := loginBlock(t, l)
	if m, _ := top.(map[string]any); m["url"] != loginLink || m["wants_code"] != true {
		t.Errorf("top-level login: %v", top)
	}
	// the helper runs in the sign-in container with the shared folder as its sign-in storage
	real, _ := filepath.EvalSymlinks(agent.SignInDirOf(l.h.Dir))
	if len(box.argv) != 1 || box.argv[0] != agent.LoginHelper {
		t.Errorf("argv %v", box.argv)
	}
	if box.env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != real || box.env["CLAUDE_CONFIG_DIR"] != "/tmp/claude-login" {
		t.Errorf("env %v (want %s)", box.env, real)
	}
	// a second start while one runs
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("second start: %v", err)
	}
	// a code with a space is refused before it reaches the helper
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "has space"}); err == nil {
		t.Error("a code with a space was sent")
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "CODE-9876-xyz"}); err != nil {
		t.Fatal(err)
	}
	step = waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "done" })
	if step["action"] != "login_again" || step["url"] != nil {
		t.Errorf("after the code: %v", step)
	}
	for _, r := range step["projects"].([]any) {
		if r.(map[string]any)["state"] != "done" {
			t.Errorf("a role is not signed in: %v", r)
		}
	}
	if box.closed != 1 {
		t.Errorf("the sign-in container was closed %d times", box.closed)
	}
	// the code and the credential appear nowhere in what the status says
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "CODE-9876-xyz") || strings.Contains(string(b), "sk-ant") {
		t.Errorf("a secret in the status: %s", b)
	}
	// no sign-in runs any more
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "CODE-9876-xyz"}); err == nil || !strings.Contains(err.Error(), "no sign-in") {
		t.Errorf("a code with no sign-in: %v", err)
	}
}

func TestT3LoginFailureAndCancel(t *testing.T) {
	l, box, _, _ := loginLife(t)
	box.script = func(b *loginBox, c *loginConn) {
		c.say("URL " + loginLink)
		c.say("FAILED the code was refused")
		c.finish()
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "failed" })
	if !strings.Contains(step["reason"].(string), "the code was refused") || step["action"] != "login" {
		t.Errorf("failure: %v", step)
	}
	// try again, and cancel before the code
	box.script = goodScript("never")
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	res, err := handle(t, l.h, "tok", "t3-setup.cancel", nil)
	if err != nil || res["stopped"] != true {
		t.Fatalf("cancel: %v %v", res, err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["state"] != "working" })
	step, top := loginBlock(t, l)
	if step["state"] != "todo" || top != nil {
		t.Errorf("after cancel: %v %v", step, top)
	}
	waitUntil(t, func() bool { box.mu.Lock(); defer box.mu.Unlock(); return box.closed == 2 })
	if agent.SignedInAt(box.dir) {
		t.Error("signed in without a code")
	}
}

func TestT3LoginRefusals(t *testing.T) {
	l, box, _, _ := loginLife(t)
	for name, args := range map[string]map[string]any{
		"unknown role":   {"step": "login", "method": "claudeai", "role": "r_unknown1"},
		"unknown method": {"step": "login", "method": "console"},
	} {
		if _, err := handle(t, l.h, "tok", "t3-setup.run", args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if box.starts != 0 {
		t.Errorf("%d containers started for refused requests", box.starts)
	}
	box.startErr = errors.New("the container tool isn't installed")
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "failed" })
	if !strings.Contains(step["reason"].(string), "isn't installed") {
		t.Errorf("reason: %v", step["reason"])
	}
	// a check (no method) starts nothing
	n := box.starts
	if res, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login"}); err != nil || res["started"] != true || box.starts != n {
		t.Errorf("check: %v %v (%d starts)", res, err, box.starts)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.login.open", nil); err == nil {
		t.Error("open with no sign-in")
	}
}

// login.open returns the sign-in link for the panel. It does not open a browser on this computer.
func TestT3LoginOpenReturnsTheLink(t *testing.T) {
	l, _, op, _ := loginLife(t)
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	res, err := handle(t, l.h, "tok", "t3-setup.login.open", nil)
	if err != nil || res["url"] != loginLink || res["opened"] != false {
		t.Fatalf("%v %v", res, err)
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if len(op.opened) != 0 || len(op.runs) != 0 {
		t.Errorf("the home computer opened a browser: %v, commands run: %v", op.opened, op.runs)
	}
}

// A link to anywhere but a sign-in host is not shown or opened.
func TestT3LoginRefusesAForeignLink(t *testing.T) {
	l, box, op, _ := loginLife(t)
	box.script = func(b *loginBox, c *loginConn) {
		c.say("URL https://evil.example.com/login")
		<-c.done
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "failed" })
	if step["url"] != nil || !strings.Contains(step["reason"].(string), "doesn't point to a Claude") {
		t.Errorf("foreign link: %v", step)
	}
	if len(op.runs)+len(op.opened) != 0 {
		t.Errorf("opened %v %v", op.runs, op.opened)
	}
}

// Home decides "signed in" by stat: the credential file has to be there and not empty; its content is not looked at.
func TestT3LoginStateComesFromTheFolderOnly(t *testing.T) {
	l, _, _, _ := loginLife(t)
	dir := agent.SignInDirOf(l.h.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	step, _ := loginBlock(t, l)
	reason, _ := step["reason"].(string)
	if step["state"] != "todo" {
		t.Errorf("empty folder: %v", step)
	}
	if strings.Contains(reason, "Waiting for earlier steps") {
		if step["action"] != "" {
			t.Errorf("sign-in offered while earlier steps are not done: %v", step)
		}
	} else if !strings.Contains(reason, "No credential file yet") {
		t.Errorf("empty folder: %v", step)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.CredentialFile), []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if step, _ := loginBlock(t, l); step["state"] != "done" || step["action"] != "login_again" || !strings.Contains(step["reason"].(string), "A credential file is present") ||
		strings.Contains(strings.ToLower(step["reason"].(string)), "signed in") {
		t.Errorf("unreadable credential file: %v", step)
	}
	_ = os.Chmod(filepath.Join(dir, agent.CredentialFile), 0o600)
}

var _ = time.Second

// sweepRecorder is a backend that notes the sweep.
type sweepRecorder struct {
	agent.VMBackend
	swept bool
}

func (r *sweepRecorder) Sweep(context.Context) { r.swept = true }

// Rubi Home's start sweeps the sign-in VMs a crash left, the T3 roles' as well as the runner path's: SweepAgent asks the
// backend to sweep (the Apple backend's Sweep removes both kinds, see the agent package's test of it).
func TestSweepAgentSweepsBothSignInContainers(t *testing.T) {
	h, _, _ := agentHelper(t)
	rec := &sweepRecorder{VMBackend: agent.NoBackend("test")}
	h.backendOverride = func() agent.VMBackend { return rec }
	h.SweepAgent(context.Background())
	if !rec.swept {
		t.Fatal("SweepAgent did not ask the backend to sweep")
	}
}

// The code the user pastes goes to the helper and nowhere else: not into Home's log, not into the progress lines or any
// other part of the status, and not into the error of a refused one.
func TestT3LoginCodeIsNeverLogged(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); logs.Write(p); mu.Unlock(); return len(p), nil }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	l, box, _, _ := loginLife(t)
	const secret = "SECRETCODE-zq81-xyz"
	box.script = goodScript(secret)
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	var all []string
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": secret + " with a space"}); err == nil {
		t.Fatal("a code with a space was sent")
	} else {
		all = append(all, err.Error())
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": secret}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "done" })
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	b, _ := json.Marshal(st)
	all = append(all, string(b), fmt.Sprint(step["progress"]))
	mu.Lock()
	all = append(all, logs.String())
	mu.Unlock()
	for _, text := range all {
		if strings.Contains(text, secret) || strings.Contains(text, secret[:12]) {
			t.Errorf("the code appears in %q", text)
		}
	}
	// a wrong code shown by the helper is not echoed either
	box.script = func(b *loginBox, c *loginConn) {
		c.say("URL " + loginLink)
		c.say("CODE?")
		for l := range c.lines {
			c.say("FAILED " + "the code was wrong")
			_ = l
			c.finish()
			return
		}
	}
	_ = os.Remove(filepath.Join(box.dir, agent.CredentialFile))
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "WRONGCODE-1234"}); err != nil {
		t.Fatal(err)
	}
	step = waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "failed" })
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logs.String(), "WRONGCODE") || strings.Contains(fmt.Sprint(step), "WRONGCODE") {
		t.Errorf("a wrong code was echoed: %v", step)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A link with something credential-shaped in it is not shown or opened, even on a sign-in host.
func TestT3LoginRefusesALinkThatCarriesACredential(t *testing.T) {
	l, box, op, _ := loginLife(t)
	box.script = func(b *loginBox, c *loginConn) {
		c.say("URL https://claude.ai/cai/oauth/authorize?state=sk-ant-oat01-FAKEFAKEFAKEFAKEFAKE")
		<-c.done
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "failed" })
	b, _ := json.Marshal(step)
	if step["url"] != nil || strings.Contains(string(b), "sk-ant") {
		t.Errorf("the link was shown: %s", b)
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.login.open", nil); err == nil || len(op.runs)+len(op.opened) != 0 {
		t.Errorf("open: %v %v %v", err, op.runs, op.opened)
	}
}

// The earlier design's token folder is removed when Home starts, so no stale subscription token stays on disk; only a plain
// folder of Home's own directory is removed, never what a link points to.
func TestStartRemovesTheEarlierDesignsTokenFolder(t *testing.T) {
	old := sweepSignIn
	sweepSignIn = func(agent.VMBackend, context.Context) {}
	defer func() { sweepSignIn = old }()
	h, _, _ := agentHelper(t)
	legacy := filepath.Join(h.Dir, "agent-auth")
	if err := os.MkdirAll(filepath.Join(legacy, "token"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "token", "some-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	signin := agent.SignInDirOf(h.Dir)
	if err := os.MkdirAll(signin, 0o700); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(signin, agent.CredentialFile)
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.SweepAgent(context.Background())
	if _, err := os.Lstat(legacy); err == nil {
		t.Error("the old token folder is still there")
	}
	if _, err := os.Stat(cred); err != nil {
		t.Errorf("the new sign-in was touched: %v", err)
	}
	// a link is not followed
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, legacy); err != nil {
		t.Fatal(err)
	}
	h.SweepAgent(context.Background())
	if _, err := os.Stat(filepath.Join(elsewhere, "keep")); err != nil {
		t.Errorf("what a link points to was removed: %v", err)
	}
	// a plain file of that name is not a folder: left alone
	os.Remove(legacy)
	if err := os.WriteFile(legacy, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.SweepAgent(context.Background())
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("a file was removed: %v", err)
	}
}

// The code handler only writes the code. When the helper then saves the credential, the same
// AfterSignIn path the sandbox uses starts the self-test. A failed sign-in does not.
func TestSignInCompletionStartsTheSelfTest(t *testing.T) {
	l, box, _, _ := loginLife(t)
	var started atomic.Int32
	l.h.T3.LoginOptions = func(o *setup.Options) {
		orig := o.OnSignedIn
		if orig == nil {
			t.Errorf("sign-in completion has no OnSignedIn")
		}
		o.OnSignedIn = func() {
			started.Add(1)
			if orig != nil {
				orig()
			}
		}
	}
	box.script = func(b *loginBox, c *loginConn) {
		c.say("URL " + loginLink)
		c.say("CODE?")
		if code := <-c.lines; code != "CODE-9876-xyz" {
			c.say("FAILED unexpected code")
			c.finish()
			return
		}
		time.Sleep(400 * time.Millisecond)
		_ = os.MkdirAll(b.dir, 0o700)
		_ = os.WriteFile(filepath.Join(b.dir, agent.CredentialFile), []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-FAKE"}}`), 0o000)
		c.say("SAVED")
		c.finish()
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["wants_code"] == true })
	if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "CODE-9876-xyz"}); err != nil {
		t.Fatal(err)
	}
	step := waitLogin(t, l, func(s map[string]any) bool { return s["reason"] == setup.SigningIn })
	if step["state"] != "working" || step["url"] != nil {
		t.Fatalf("while the helper finishes: %#v", step)
	}
	waitLogin(t, l, func(s map[string]any) bool { return s["state"] == "done" })
	waitUntil(t, func() bool { return started.Load() == 1 })
	waitUntil(t, func() bool {
		st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
		if err != nil {
			return false
		}
		self := stepByID(t, st, "selftest")
		return self["state"] == "working" || self["state"] == "failed" || self["state"] == "done"
	})
	if started.Load() != 1 {
		t.Fatalf("self-test starts: %d", started.Load())
	}

	l2, box2, _, _ := loginLife(t)
	var failed atomic.Int32
	l2.h.T3.LoginOptions = func(o *setup.Options) {
		orig := o.OnSignedIn
		o.OnSignedIn = func() {
			failed.Add(1)
			if orig != nil {
				orig()
			}
		}
	}
	box2.script = func(b *loginBox, c *loginConn) {
		c.say("URL " + loginLink)
		c.say("FAILED the code was refused")
		c.finish()
	}
	if _, err := handle(t, l2.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	waitLogin(t, l2, func(s map[string]any) bool { return s["state"] == "failed" })
	time.Sleep(50 * time.Millisecond)
	if failed.Load() != 0 {
		t.Fatalf("a failed sign-in started the self-test %d times", failed.Load())
	}
}

func TestSelfTestQueueSurvivesTheJobEndingInTheGap(t *testing.T) {
	l, _, _, _ := loginLife(t)
	l.h.T3.mu.Lock()
	l.h.T3.job = &t3Job{Step: T3StepInstall, Started: time.Now()}
	l.h.T3.mu.Unlock()
	var gapErr string
	l.h.T3.onBusyGap = func() {
		// The busy step ends in the gap after it was observed. The queue is already set,
		// so this start is not missed.
		l.h.T3.opMu.Lock()
		l.h.T3.finishJob(T3StepInstall, nil)
		l.h.T3.mu.Lock()
		switch {
		case l.h.T3.pendingSelf:
			gapErr = "the self-test was still only queued after the busy step ended"
		case l.h.T3.job == nil || l.h.T3.job.Step != T3StepSelfTest:
			gapErr = fmt.Sprintf("self-test did not start: %#v", l.h.T3.job)
		}
		l.h.T3.mu.Unlock()
		l.h.T3.opMu.Unlock()
	}
	if err := l.h.T3.startSelfTestAfterSignIn(); err != nil {
		t.Fatal(err)
	}
	if gapErr != "" {
		t.Fatal(gapErr)
	}
	if !errors.Is(&setupBusyError{Step: T3StepInstall}, errSetupBusy) {
		t.Fatal("busy error is not errSetupBusy")
	}
}

func TestSelfTestQueuesBehindABusyStep(t *testing.T) {
	l, _, _, _ := loginLife(t)
	l.h.T3.mu.Lock()
	l.h.T3.job = &t3Job{Step: T3StepInstall, Started: time.Now()}
	l.h.T3.mu.Unlock()
	if err := l.h.T3.startSelfTestAfterSignIn(); err != nil {
		t.Fatal(err)
	}
	st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	self := stepByID(t, st, "selftest")
	if !strings.Contains(fmt.Sprint(self["reason"]), "other setup step") || self["state"] == "failed" {
		t.Fatalf("queued self-test: %#v", self)
	}
	l.h.T3.finishJob(T3StepInstall, nil)
	waitUntil(t, func() bool {
		st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
		if err != nil {
			return false
		}
		self = stepByID(t, st, "selftest")
		s, _ := self["state"].(string)
		return s == "working" || s == "failed" || s == "done"
	})
}
