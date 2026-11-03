package home

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// makeDirAndCopy plays the root script: it makes the missing directory, then copies.
func makeDirAndCopy(ctx context.Context, p InstallPlan) error {
	if err := os.MkdirAll(filepath.Dir(p.Dst), 0o755); err != nil {
		return err
	}
	return copyRunner(nil)(ctx, p)
}

func hookHelper(t *testing.T) (*Helper, string) {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: tokenHash("tok")}} }); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h.hookPath = filepath.Join(dir, "bin", "rubi-home")
	h.pluginAt = filepath.Join(dir, "plugin")
	h.hookOpts = func(o *InstallOptions) { o.RequireRoot = false; o.Run = makeDirAndCopy }
	return h, dir
}

// helloFresh is helloHook without the 10 s cache: the tests rewrite the file faster than its mtime moves.
func helloFresh(t *testing.T, h *Helper) map[string]any {
	t.Helper()
	h.hookMu.Lock()
	h.hookSeen = hookCached{}
	h.hookMu.Unlock()
	return helloHook(t, h)
}

func helloHook(t *testing.T, h *Helper) map[string]any {
	t.Helper()
	v, err := h.Handle(context.Background(), homeproto.Envelope{Op: "hello", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	hook, ok := v.(map[string]any)["hook"].(map[string]any)
	if !ok {
		t.Fatalf("hello has no hook: %v", v)
	}
	return hook
}

func TestHelloReportsTheHookState(t *testing.T) {
	h, dir := hookHelper(t)
	if err := os.MkdirAll(filepath.Dir(h.hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	secure := func(b *HookBinary) { b.Secure, b.WhyInsecure = true, "" } // the temp file is the test user's, not root's
	h.hookTweak = secure

	withVersions(t, "", "", "v0.14.0")
	if got := helloFresh(t, h)["state"]; got != HookMissing {
		t.Fatalf("no file: %v", got)
	}
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo v0.14.0")
	hk := helloFresh(t, h)
	if hk["state"] != HookCurrent || hk["version"] != "v0.14.0" || hk["plugin"] != false {
		t.Fatalf("current: %v", hk)
	}
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo v0.13.0")
	if got := helloFresh(t, h)["state"]; got != HookStale {
		t.Fatalf("older: %v", got)
	}
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo dev")
	if got := helloFresh(t, h)["state"]; got != HookDev {
		t.Fatalf("dev hook: %v", got)
	}
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo v0.14.0")
	withVersions(t, "", "", "dev")
	if got := helloFresh(t, h)["state"]; got != HookDev {
		t.Fatalf("dev helper: %v", got)
	}
	withVersions(t, "", "", "v0.14.0")
	script(t, filepath.Dir(h.hookPath), "rubi-home", "exit 3")
	if got := helloFresh(t, h)["state"]; got != HookStale {
		t.Fatalf("unreadable version: %v", got)
	}
	// Not root-owned (no tweak): insecure, whatever its version.
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo v0.14.0")
	h.hookTweak = nil
	if os.Getuid() != 0 {
		if got := helloFresh(t, h)["state"]; got != HookInsecure {
			t.Fatalf("user-owned: %v", got)
		}
	}
	// The plugin is reported when its folder exists.
	os.MkdirAll(h.pluginAt, 0o755)
	if helloFresh(t, h)["plugin"] != true {
		t.Fatal("plugin not reported")
	}
	_ = dir
}

func TestHookStateNeverFailsHello(t *testing.T) {
	h, _ := hookHelper(t)
	h.hookPath = "/proc/does/not/exist/\x00x" // an unusable path is a state, not an error
	hk := helloHook(t, h)
	if s, _ := hk["state"].(string); s != HookMissing && s != HookInsecure {
		t.Fatalf("%v", hk)
	}
	b, _ := json.Marshal(hk)
	if strings.Contains(string(b), "proc") {
		t.Fatalf("the hello holds a path: %s", b)
	}
}

func hookInstallFixture(t *testing.T) (*Helper, string) {
	t.Helper()
	old := version.Version
	version.Version = "v0.14.0"
	t.Cleanup(func() { version.Version = old })
	h, dir := hookHelper(t)
	h.hookTweak = func(b *HookBinary) { b.Secure = true }
	src := script(t, t.TempDir(), "src", "echo v0.14.0")
	prev := h.hookOpts
	h.hookOpts = func(o *InstallOptions) { prev(o); o.Src = src }
	return h, dir
}

// runWith replaces the administrator step of the next installations.
func runWith(h *Helper, run func(ctx context.Context, p InstallPlan) error) {
	prev := h.hookOpts
	h.hookOpts = func(o *InstallOptions) { prev(o); o.Run = run }
}

// waitIdle polls hello until the background installation ended, and returns the hook part.
func waitIdle(t *testing.T, h *Helper) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		hk := helloFresh(t, h)
		if hk["installing"] == false {
			return hk
		}
		if time.Now().After(deadline) {
			t.Fatalf("still installing: %v", hk)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lastOf(t *testing.T, hk map[string]any) map[string]any {
	t.Helper()
	l, ok := hk["last"].(map[string]any)
	if !ok {
		t.Fatalf("no last result: %v", hk)
	}
	return l
}

func TestHookInstallStartsAtOnceAndTheResultComesInHello(t *testing.T) {
	h, _ := hookInstallFixture(t)
	var runs int
	var ran InstallPlan
	runWith(h, func(ctx context.Context, p InstallPlan) error { runs++; ran = p; return makeDirAndCopy(ctx, p) })
	got, err := handle(t, h, "tok", "hook.install", map[string]any{})
	if err != nil || got["started"] != true {
		t.Fatalf("%v %v", got, err)
	}
	hk := waitIdle(t, h)
	if l := lastOf(t, hk); l["result"] != HookResultOK {
		t.Fatalf("last: %v", l)
	}
	if runs != 1 || hk["state"] != HookCurrent || hk["version"] != "v0.14.0" {
		t.Fatalf("runs %d, hook %v", runs, hk)
	}
	// The directory did not exist: the plan held a script that makes it.
	if !strings.Contains(ran.Script, "mkdir -p") || ran.Plugin != nil {
		t.Fatalf("plan: plugin %v\n%s", ran.Plugin, ran.Script)
	}
	if _, err := os.Stat(h.hookPath); err != nil {
		t.Fatal(err)
	}
}

// The administrator dialog outlives the request that started it (the relay gives a handler 60 s): the
// installation runs on its own context, and hook.install returns while the dialog is still open.
func TestHookInstallDoesNotRunOnTheRequestContext(t *testing.T) {
	h, _ := hookInstallFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var ctxErr atomic.Value
	runWith(h, func(ctx context.Context, p InstallPlan) error {
		close(entered)
		<-release
		ctxErr.Store(fmt.Sprint(ctx.Err()))
		return makeDirAndCopy(ctx, p)
	})
	reqCtx, cancel := context.WithCancel(context.Background())
	b, _ := json.Marshal(map[string]any{})
	v, err := h.Handle(reqCtx, homeproto.Envelope{Op: "hook.install", Token: "tok", Args: b})
	if err != nil || v.(map[string]any)["started"] != true {
		t.Fatalf("%v %v", v, err)
	}
	<-entered
	if hk := helloHook(t, h); hk["installing"] != true {
		t.Fatalf("not installing while the dialog is open: %v", hk)
	}
	cancel() // the request is over
	close(release)
	hk := waitIdle(t, h)
	if got := ctxErr.Load(); got != "<nil>" {
		t.Fatalf("the installation's context ended with the request: %v", got)
	}
	if lastOf(t, hk)["result"] != HookResultOK {
		t.Fatalf("%v", hk)
	}
}

func TestHookInstallTimeoutIsClear(t *testing.T) {
	h, _ := hookInstallFixture(t)
	h.hookLimit = 50 * time.Millisecond
	runWith(h, func(ctx context.Context, p InstallPlan) error {
		<-ctx.Done()
		return errors.New("osascript: signal: killed") // what a killed osascript says
	})
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	l := lastOf(t, waitIdle(t, h))
	msg, _ := l["message"].(string)
	if l["result"] != HookResultTimeout || !strings.Contains(msg, "timed out") || strings.Contains(msg, "killed") {
		t.Fatalf("last: %v", l)
	}
}

func TestHookInstallCancelledAndFailedAreClear(t *testing.T) {
	h, _ := hookInstallFixture(t)
	runWith(h, func(context.Context, InstallPlan) error { return ErrAdminCancelled })
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	if l := lastOf(t, waitIdle(t, h)); l["result"] != HookResultCancelled {
		t.Fatalf("cancel: %v", l)
	}
	// A failure keeps no path of the user's in what the panel gets.
	h2, _ := hookInstallFixture(t)
	home, _ := os.UserHomeDir()
	runWith(h2, func(context.Context, InstallPlan) error { return errors.New("boom in " + home + "/x " + h2.hookPath) })
	if _, err := handle(t, h2, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	l := lastOf(t, waitIdle(t, h2))
	b, _ := json.Marshal(l)
	if l["result"] != HookResultFailed || strings.Contains(string(b), h2.hookPath) || strings.Contains(string(b), "boom") ||
		(home != "" && home != "/" && strings.Contains(string(b), home)) {
		t.Fatalf("failure: %s", b)
	}
	// A failure is no reason to wait.
	if _, err := handle(t, h2, "tok", "hook.install", nil); err != nil {
		t.Fatalf("retry after a failure: %v", err)
	}
	waitIdle(t, h2)
}

func TestHookInstallNeedsAPairedToken(t *testing.T) {
	h, _ := hookInstallFixture(t)
	runWith(h, func(context.Context, InstallPlan) error { t.Error("ran without a paired token"); return nil })
	for _, op := range []string{"hook.install", "hook.plan"} {
		if _, err := handle(t, h, "wrong", op, map[string]any{}); err == nil {
			t.Fatalf("an unpaired caller used %s", op)
		}
	}
}

func TestHookInstallRunsOneAtATime(t *testing.T) {
	h, _ := hookInstallFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	runWith(h, func(ctx context.Context, p InstallPlan) error {
		close(entered)
		<-release
		return makeDirAndCopy(ctx, p)
	})
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := handle(t, h, "tok", "hook.install", nil); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second install: %v", err)
	}
	close(release)
	waitIdle(t, h)
}

// Home limits what a paired core can ask: after a dialog nobody answered (cancelled or timed out) it refuses
// to open another for two minutes; a plain failure doesn't start the wait.
func TestHookInstallIsRefusedForTwoMinutesAfterAnUnansweredDialog(t *testing.T) {
	for _, res := range []error{ErrAdminCancelled, context.DeadlineExceeded} {
		h, _ := hookInstallFixture(t)
		now := time.Now()
		h.hookClock = func() time.Time { return now }
		var runs atomic.Int32
		runWith(h, func(ctx context.Context, p InstallPlan) error {
			if runs.Add(1) == 1 {
				return res
			}
			return makeDirAndCopy(ctx, p)
		})
		if res == context.DeadlineExceeded { // a time limit that has run out
			h.hookLimit = time.Nanosecond
		}
		if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, h)
		h.hookLimit = 0
		if _, err := handle(t, h, "tok", "hook.install", nil); err == nil || !strings.Contains(err.Error(), "2 minutes") || !strings.Contains(err.Error(), CodeCooldown) {
			t.Fatalf("%v: asked again at once: %v", res, err)
		}
		if _, err := handle(t, h, "tok", "hook.plan", nil); err == nil {
			t.Fatalf("%v: hook.plan didn't refuse during the wait", res)
		}
		if runs.Load() != 1 {
			t.Fatalf("%v: the dialog opened again: %d", res, runs.Load())
		}
		now = now.Add(hookCooldown + time.Second)
		if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
			t.Fatalf("%v: still refused after the wait: %v", res, err)
		}
		if l := lastOf(t, waitIdle(t, h)); l["result"] != HookResultOK {
			t.Fatalf("%v: %v", res, l)
		}
	}
}

func TestHookInstallIsRefusedWhenNothingChanges(t *testing.T) {
	h, _ := hookInstallFixture(t)
	var runs atomic.Int32
	runWith(h, func(ctx context.Context, p InstallPlan) error { runs.Add(1); return makeDirAndCopy(ctx, p) })
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	if hk := waitIdle(t, h); hk["state"] != HookCurrent {
		t.Fatalf("%v", hk)
	}
	if _, err := handle(t, h, "tok", "hook.install", nil); err == nil || !strings.Contains(err.Error(), CodeUpToDate) {
		t.Fatalf("a current hook was installed again: %v", err)
	}
	if _, err := handle(t, h, "tok", "hook.plan", nil); err == nil {
		t.Fatal("hook.plan offered a no-op")
	}
	if runs.Load() != 1 {
		t.Fatalf("runs %d", runs.Load())
	}
}

func TestHookInstallIncludesThePluginInTheSameRun(t *testing.T) {
	h, _ := hookInstallFixture(t)
	var mu sync.Mutex
	var plans []InstallPlan
	runWith(h, func(ctx context.Context, p InstallPlan) error {
		mu.Lock()
		plans = append(plans, p)
		mu.Unlock()
		return makeDirAndCopy(ctx, p)
	})
	taken := func() []InstallPlan { mu.Lock(); defer mu.Unlock(); x := plans; plans = nil; return x }
	// Not installed and not asked for: no plugin.
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if p := taken(); len(p) != 1 || p[0].Plugin != nil {
		t.Fatalf("%d plans", len(p))
	}
	// The hook is current now, and the plugin is not installed: asking for it is a change.
	if _, err := handle(t, h, "tok", "hook.install", map[string]any{"with_plugin": true}); err != nil {
		t.Fatalf("with_plugin: %v", err)
	}
	waitIdle(t, h) // the fake runner doesn't write the plugin tree, so verification fails; the plan is what counts
	if p := taken(); len(p) != 1 || p[0].Plugin == nil || !strings.Contains(p[0].FullScript(), p[0].Plugin.Script) {
		t.Fatalf("with_plugin ignored: %d plans", len(p))
	}
	// Installed already (and not the embedded files): updated in the same single run, one dialog.
	if err := os.MkdirAll(h.pluginAt, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, h, "tok", "hook.install", nil); err != nil {
		t.Fatalf("stale plugin: %v", err)
	}
	waitIdle(t, h)
	if p := taken(); len(p) != 1 || p[0].Plugin == nil {
		t.Fatalf("an installed plugin wasn't updated in the run: %d plans", len(p))
	}
}

// The target is /usr/local/bin/rubi-home and nothing else: $RUBI_HOOK_BINARY only points `rubi-home status`
// elsewhere. The state in hello is the state of the file hook.install would write.
func TestHookInstallTargetIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("RUBI_HOOK_BINARY", filepath.Join(t.TempDir(), "elsewhere"))
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := h.hookTarget(); got != DefaultHookBinary {
		t.Fatalf("target %q", got)
	}
	o, err := h.installOptions(false)
	if err != nil || o.Dst != DefaultHookBinary {
		t.Fatalf("install target %q %v", o.Dst, err)
	}
	if HookBinaryPath() == DefaultHookBinary {
		t.Fatal("the environment no longer reaches status")
	}
	// What hello inspects is what is installed: with a test seam path, the env file is never looked at.
	h2, _ := hookInstallFixture(t)
	other := script(t, t.TempDir(), "other", "echo v9.9.9")
	t.Setenv("RUBI_HOOK_BINARY", other)
	if got := helloFresh(t, h2); got["state"] != HookMissing {
		t.Fatalf("hello followed the environment: %v", got)
	}
	if h2.hookTarget() != h2.hookPath {
		t.Fatal("the test seam isn't the target")
	}
	var dst string
	runWith(h2, func(ctx context.Context, p InstallPlan) error { dst = p.Dst; return makeDirAndCopy(ctx, p) })
	if _, err := handle(t, h2, "tok", "hook.install", nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h2)
	if dst != h2.hookPath {
		t.Fatalf("installed to %q", dst)
	}
}

func TestHookPlanShowsWhatWillBeInstalledWithoutAsking(t *testing.T) {
	h, _ := hookInstallFixture(t)
	runWith(h, func(context.Context, InstallPlan) error {
		t.Error("hook.plan opened the administrator dialog")
		return nil
	})
	got, err := handle(t, h, "tok", "hook.plan", nil)
	if err != nil {
		t.Fatal(err)
	}
	o, _ := h.installOptions(false)
	want, _ := fileSHA256(o.Src)
	if got["target"] != h.hookPath || got["version"] != "v0.14.0" || got["sha256"] != want || len(want) != 64 {
		t.Fatalf("%v (want sha %s)", got, want)
	}
	if hk := helloFresh(t, h); hk["installing"] != false || hk["last"] != nil {
		t.Fatalf("hook.plan changed the state: %v", hk)
	}
}

// What core and the panel get on a refusal is a code and a fixed sentence; the paths stay in the log.
func TestHookRefusalsCarryACodeAndNoPath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root's temp directory is safe")
	}
	h, _ := hookInstallFixture(t)
	prev := h.hookOpts
	h.hookOpts = func(o *InstallOptions) { prev(o); o.RequireRoot = true } // the test user's folders are not root's
	for _, op := range []string{"hook.install", "hook.plan"} {
		_, err := handle(t, h, "tok", op, nil)
		if err == nil || !strings.Contains(err.Error(), CodeUnsafeDir) || !strings.Contains(err.Error(), "isn't protected") {
			t.Fatalf("%s: %v", op, err)
		}
		if strings.Contains(err.Error(), filepath.Dir(h.hookPath)) || strings.Contains(err.Error(), os.TempDir()) {
			t.Fatalf("%s: a path in %q", op, err)
		}
	}
	if code := codeOf(codedf(CodeNotPlain, "x")); code != CodeNotPlain {
		t.Fatal(code)
	}
	// Every code has its own sentence, none of them with a path or a placeholder.
	for _, c := range []string{CodeBadPath, CodeNoSource, CodeNotDir, CodeNotPlain, CodeUnsafeDir, CodeSelf, CodePlugin, CodeVerify,
		CodeCooldown, CodeUpToDate, CodeBusy, CodeBadRequest, ""} {
		if s := hookSentence(c); s == "" || strings.ContainsAny(s, "/%") {
			t.Errorf("%q: %q", c, s)
		}
	}
}

func TestHelloSaysWhyAnInsecureHookIsInsecureAndWhetherItCanBeFixed(t *testing.T) {
	h, _ := hookInstallFixture(t)
	h.hookTweak = nil
	dir := filepath.Dir(h.hookPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink: macOS can't be asked to fix it from here.
	real := script(t, t.TempDir(), "real", "echo v0.14.0")
	if err := os.Symlink(real, h.hookPath); err != nil {
		t.Fatal(err)
	}
	hk := helloFresh(t, h)
	if hk["state"] != HookInsecure || hk["why"] != "symlink" || hk["fixable"] != false {
		t.Fatalf("symlink: %v", hk)
	}
	os.Remove(h.hookPath)
	// Not root's: the install replaces it.
	if os.Getuid() != 0 {
		script(t, dir, "rubi-home", "echo v0.14.0")
		hk = helloFresh(t, h)
		if hk["state"] != HookInsecure || hk["why"] != "owner" || hk["fixable"] != true {
			t.Fatalf("owner: %v", hk)
		}
		// In a folder that is not root's it can't be fixed (the install would refuse): the test user's folders.
		prev := h.hookOpts
		h.hookOpts = func(o *InstallOptions) { prev(o); o.RequireRoot = true }
		hk = helloFresh(t, h)
		if hk["fixable"] != false {
			t.Fatalf("unsafe dir: %v", hk)
		}
	}
	// A folder above that anyone but root can change.
	h.hookTweak = func(b *HookBinary) { b.Secure, b.WhyInsecure = false, "x is writable by group or others" }
	if hk = helloFresh(t, h); hk["why"] != "dir" {
		t.Fatalf("dir: %v", hk)
	}
	// Nothing about a secure hook.
	h.hookTweak = func(b *HookBinary) { b.Secure = true }
	h.hookOpts = func(o *InstallOptions) {}
	if hk = helloFresh(t, h); hk["why"] != nil {
		t.Fatalf("why for a secure hook: %v", hk)
	}
}

// hello must stay cheap: no SHA-256 of the file, `version` only on a Secure file, and the inspection is kept
// for 10 s while the file looks the same.
func TestHelloIsCheap(t *testing.T) {
	h, _ := hookInstallFixture(t)
	dir := filepath.Dir(h.hookPath)
	os.MkdirAll(dir, 0o755)
	counter := filepath.Join(t.TempDir(), "runs")
	script(t, dir, "rubi-home", `echo run >> '`+counter+`'; echo v0.14.0`)
	runs := func() int {
		b, _ := os.ReadFile(counter)
		return strings.Count(string(b), "run")
	}
	if b := h.inspectTarget(true); b.SHA256 != "" {
		t.Fatalf("hello hashed the file: %+v", b)
	}
	base := runs()
	now := time.Now()
	h.hookClock = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		helloHook(t, h)
	}
	if runs() != base {
		t.Fatalf("version ran again while cached: %d vs %d", runs(), base)
	}
	// The file changes (size): inspected again.
	script(t, dir, "rubi-home", `echo run >> '`+counter+`'; echo v0.14.0 # changed`)
	helloHook(t, h)
	if runs() != base+1 {
		t.Fatalf("a changed file wasn't inspected again: %d vs %d", runs(), base)
	}
	// The cache expires.
	now = now.Add(hookCacheTTL + time.Second)
	helloHook(t, h)
	if runs() != base+2 {
		t.Fatalf("an old inspection was kept: %d vs %d", runs(), base)
	}
	// Not Secure: `version` is not run on it.
	if os.Getuid() != 0 {
		h.hookTweak = nil
		now = now.Add(time.Hour)
		before := runs()
		if hk := helloHook(t, h); hk["state"] != HookInsecure || runs() != before {
			t.Fatalf("version ran on an insecure binary: %v", hk)
		}
	}
}
