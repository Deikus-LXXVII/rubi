package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// ---- the plan's hash ----

func hashFixture(t *testing.T) (src, dst string, tools installTools) {
	t.Helper()
	src, dst = installFixture(t)
	return src, dst, hostTools(t)
}

// The hash covers everything root would run: a changed binary, target or plugin file changes it, the same plan does not.
func TestPlanSHA256CoversBinaryTargetAndPlugin(t *testing.T) {
	src, dst, tools := hashFixture(t)
	plan := func(dst string, plug bool) string {
		t.Helper()
		o := InstallOptions{Src: src, Dst: dst, WithPlugin: plug, PluginDst: filepath.Join(filepath.Dir(dst), "claude-plugin"), tools: &tools}
		p, err := fullPlan(o)
		if err != nil {
			t.Fatal(err)
		}
		return PlanSHA256(p)
	}
	base := plan(dst, false)
	if len(base) != 64 || plan(dst, false) != base {
		t.Fatalf("not a stable SHA-256: %q", base)
	}
	if plan(dst, true) == base {
		t.Error("the plugin is not in the hash")
	}
	if plan(filepath.Join(filepath.Dir(dst), "other"), false) == base {
		t.Error("the target is not in the hash")
	}
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if plan(dst, false) == base {
		t.Error("the binary is not in the hash")
	}
}

// The approval's hash is checked by the root side: a binary swapped after the approval, or a different expectation,
// and nothing is run.
func TestInstallHookRunsOnlyThePlanThatWasApproved(t *testing.T) {
	src, dst, tools := hashFixture(t)
	o := InstallOptions{Src: src, Dst: dst, tools: &tools}
	approved, err := fullPlan(o)
	if err != nil {
		t.Fatal(err)
	}
	h := PlanSHA256(approved)

	var ran []InstallPlan
	run := func(_ context.Context, p InstallPlan) error {
		ran = append(ran, p)
		return copyRunner(nil)(context.Background(), p)
	}
	o.Run, o.ExpectPlanSHA256 = run, h
	if err := InstallHook(context.Background(), &strings.Builder{}, o); err != nil || len(ran) != 1 || PlanSHA256(ran[0]) != h {
		t.Fatalf("the approved plan: %v, ran %d", err, len(ran))
	}

	// 1. the binary is swapped after the approval
	ran = nil
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho swapped\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err = InstallHook(context.Background(), &strings.Builder{}, o)
	if codeOf(err) != CodePlanChanged || len(ran) != 0 {
		t.Fatalf("a swapped binary: %v, ran %d", err, len(ran))
	}
	// 2. the hash is of another plan, or not a hash at all
	for _, bad := range []string{strings.Repeat("0", 64), "", " ", "abc", strings.ToUpper(h) + "x"} {
		if bad == "" {
			continue // no expectation: the interactive path
		}
		o.ExpectPlanSHA256 = bad
		if err := InstallHook(context.Background(), &strings.Builder{}, o); codeOf(err) != CodePlanChanged || len(ran) != 0 {
			t.Errorf("hash %q: %v, ran %d", bad, err, len(ran))
		}
	}
	// 3. a dry run with a wrong hash refuses too (it would show a plan that is not the approved one)
	o.DryRun, o.ExpectPlanSHA256 = true, strings.Repeat("0", 64)
	if err := InstallHook(context.Background(), &strings.Builder{}, o); codeOf(err) != CodePlanChanged {
		t.Errorf("dry run: %v", err)
	}
}

// ---- the plugin's .mcp.json ----

func TestRenderPluginFileReplacesOnlyTheHookCommand(t *testing.T) {
	mcp := []byte("{\n  \"mcpServers\": {\n    \"rubi\": {\n      \"command\": \"/usr/local/bin/rubi-home\",\n      \"args\": [\"claude-mcp\"]\n    }\n  }\n}\n")
	// macOS and Linux: byte for byte
	if got := renderPluginFile("rubi-sisters/.mcp.json", mcp, "/usr/local/bin/rubi-home"); string(got) != string(mcp) {
		t.Fatalf("changed on Unix: %q", got)
	}
	win := `C:\Program Files\Rubi Home\rubi-home.exe`
	got := renderPluginFile("rubi-sisters/.mcp.json", mcp, win)
	var m struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got, &m); err != nil || m.MCPServers["rubi"].Command != win || !reflect.DeepEqual(m.MCPServers["rubi"].Args, []string{"claude-mcp"}) {
		t.Fatalf("%v %+v\n%s", err, m, got)
	}
	// other files are left alone, even if they mention the path
	other := []byte("see /usr/local/bin/rubi-home\n")
	if got := renderPluginFile("rubi-sisters/skills/x/SKILL.md", other, win); string(got) != string(other) {
		t.Errorf("a skill file changed: %q", got)
	}
	// the plugin the binary carries has the hook path of this computer in its .mcp.json
	files, err := EmbeddedPluginFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range files {
		if filepath.Base(f.Path) == ".mcp.json" {
			found = true
			var c struct {
				MCPServers map[string]struct{ Command string } `json:"mcpServers"`
			}
			if err := json.Unmarshal(f.Data, &c); err != nil || c.MCPServers["rubi"].Command != DefaultHookBinary {
				t.Errorf("embedded .mcp.json: %v %+v (hook %s)", err, c, DefaultHookBinary)
			}
		}
	}
	if !found {
		t.Error("no .mcp.json in the plugin")
	}
}

// ---- the headless path ----

// fakeElev is an Elevator that cannot ask anyone (or can), and records what it was asked to run.
type fakeElev struct {
	mu       sync.Mutex
	avail    error
	runErr   error
	runs     []InstallPlan
	describe string
}

func (f *fakeElev) Describe() string { return "a fake elevator" }
func (f *fakeElev) Sentence() string {
	return "A fake prompt will appear; approve only if you asked for this"
}
func (f *fakeElev) Available(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.avail
}
func (f *fakeElev) Run(_ context.Context, p InstallPlan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, p)
	return f.runErr
}

// noRunHelper is a Helper whose administrator step is the given Elevator (no Run override).
func noRunHelper(t *testing.T, el Elevator) (*Helper, string) {
	t.Helper()
	h, dir := hookHelper(t)
	h.hookOpts = func(o *InstallOptions) { o.RequireRoot = false }
	h.hookElev = el
	h.hookTweak = func(b *HookBinary) { b.Secure, b.WhyInsecure = true, "" }
	return h, dir
}

func hookCall(t *testing.T, h *Helper, op string, args any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	var v any
	var err error
	switch op {
	case "hook.plan":
		v, err = h.planHook(raw)
	case "hook.install":
		v, err = h.installHook(raw)
	}
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

func TestHookPlanGivesTheHashAndWhatThePersonWillSee(t *testing.T) {
	h, _ := noRunHelper(t, &fakeElev{})
	withVersions(t, "", "", "v0.14.0")
	plan, err := hookCall(t, h, "hook.plan", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := plan["plan_sha256"].(string)
	o, _ := h.installOptions(false)
	p, _ := fullPlan(o)
	if hash != PlanSHA256(p) || len(hash) != 64 || plan["admin"] != "A fake prompt will appear; approve only if you asked for this" || plan["sha256"] == hash {
		t.Fatalf("%v", plan)
	}
	// with the plugin the hash is another
	withPlug, err := hookCall(t, h, "hook.plan", map[string]any{"with_plugin": true})
	if err != nil || withPlug["plan_sha256"] == hash || withPlug["plugin"] != true {
		t.Fatalf("%v %v", withPlug, err)
	}
}

// hook.install is told the hash that was approved: another plan is refused before anything is asked or started.
func TestHookInstallRefusesAPlanThatIsNotTheApprovedOne(t *testing.T) {
	el := &fakeElev{}
	h, _ := noRunHelper(t, el)
	withVersions(t, "", "", "v0.14.0")
	plan, _ := hookCall(t, h, "hook.plan", map[string]any{})
	_, err := hookCall(t, h, "hook.install", map[string]any{"plan_sha256": strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), CodePlanChanged) {
		t.Fatalf("%v", err)
	}
	h.hookMu.Lock()
	busy := h.hookBusy
	h.hookMu.Unlock()
	if busy || len(el.runs) != 0 {
		t.Fatalf("started (%v) or ran (%d) after a refusal", busy, len(el.runs))
	}
	// the approved hash starts the installation, and the elevator is run with exactly that plan
	res, err := hookCall(t, h, "hook.install", map[string]any{"plan_sha256": plan["plan_sha256"]})
	if err != nil || res["started"] != true {
		t.Fatalf("%v %v", res, err)
	}
	waitIdle(t, h)
	if len(el.runs) != 1 || PlanSHA256(el.runs[0]) != plan["plan_sha256"] {
		t.Fatalf("runs %d", len(el.runs))
	}
}

// With nobody to ask, Home runs nothing: it answers with the exact argument list and the hash. The command is
// /usr/bin/sudo -- /bin/sh -c SCRIPT, SCRIPT is the plan's own script, and the hash is the SHA-256 of that very string, so the
// Bot (or core) can check it. Rubi Home's own binary is not in the command: nothing the user can write runs as root.
func TestHeadlessAnswersWithTheCommandAndTheHashAndRunsNothing(t *testing.T) {
	el := &fakeElev{avail: errors.Join(ErrNoAgent, errors.New("no desktop"))}
	h, _ := noRunHelper(t, el)
	withVersions(t, "", "", "v0.14.0")
	plan, _ := hookCall(t, h, "hook.plan", map[string]any{})
	for _, withPlugin := range []bool{false, true} {
		if withPlugin {
			plan, _ = hookCall(t, h, "hook.plan", map[string]any{"with_plugin": true})
		}
		res, err := hookCall(t, h, "hook.install", map[string]any{"plan_sha256": plan["plan_sha256"], "with_plugin": withPlugin})
		if err != nil {
			t.Fatal(err)
		}
		if res["needs_bot_sudo"] != true || res["started"] != false || res["plan_sha256"] != plan["plan_sha256"] || res["command"] != nil {
			t.Fatalf("%v", res)
		}
		argv, _ := res["argv"].([]string)
		o, _ := h.installOptions(withPlugin)
		full, err := fullPlan(o)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"/usr/bin/sudo", "--", "/bin/sh", "-c", full.FullScript()}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("argv %q\nwant %q", argv, want)
		}
		sum := sha256.Sum256([]byte(argv[4]))
		if hex.EncodeToString(sum[:]) != plan["plan_sha256"] || PlanSHA256(full) != plan["plan_sha256"] {
			t.Errorf("sha256(script) is not the plan hash")
		}
		for _, a := range argv[:4] {
			if strings.Contains(a, "rubi-home") || a == o.Src {
				t.Errorf("Rubi Home's binary is in the command: %q", a)
			}
		}
		h.hookMu.Lock()
		busy := h.hookBusy
		h.hookMu.Unlock()
		if busy || len(el.runs) != 0 {
			t.Fatalf("Home started or ran something (busy %v, runs %d)", busy, len(el.runs))
		}
	}
	// the refusal of a wrong hash comes first
	if _, err := hookCall(t, h, "hook.install", map[string]any{"plan_sha256": strings.Repeat("1", 64)}); err == nil {
		t.Error("headless answered for a plan that was not approved")
	}
}

// The hash is not optional: without it hook.install answers nothing (not a started installation, not a command).
func TestHookInstallNeedsTheApprovedHash(t *testing.T) {
	for _, el := range []*fakeElev{{}, {avail: ErrNoAgent}} {
		h, _ := noRunHelper(t, el)
		withVersions(t, "", "", "v0.14.0")
		for _, args := range []any{map[string]any{}, map[string]any{"plan_sha256": ""}, map[string]any{"with_plugin": true}, nil} {
			res, err := hookCall(t, h, "hook.install", args)
			if err == nil || !strings.Contains(err.Error(), CodePlanNeeded) || res != nil {
				t.Errorf("%v: %v %v", args, res, err)
			}
		}
		h.hookMu.Lock()
		busy := h.hookBusy
		h.hookMu.Unlock()
		if busy || len(el.runs) != 0 {
			t.Fatalf("started (%v) or ran (%d) without a hash", busy, len(el.runs))
		}
	}
}

// A prompt that finds nobody at run time (a polkit agent that is not there) makes the next requests go the headless way, for
// a few minutes; then the prompt is tried again.
func TestNoAgentAtRunTimeSendsTheNextRequestsTheHeadlessWayForAWhile(t *testing.T) {
	el := &fakeElev{runErr: ErrNoAgent}
	h, _ := noRunHelper(t, el)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.hookClock = func() time.Time { return now }
	withVersions(t, "", "", "v0.14.0")
	plan, _ := hookCall(t, h, "hook.plan", map[string]any{})
	args := map[string]any{"plan_sha256": plan["plan_sha256"]}
	res, err := hookCall(t, h, "hook.install", args)
	if err != nil || res["started"] != true {
		t.Fatalf("%v %v", res, err)
	}
	hk := waitIdle(t, h)
	last, _ := hk["last"].(map[string]any)
	if last["result"] != HookResultFailed || last["code"] != "no_agent" {
		t.Fatalf("last: %v", last)
	}
	res, err = hookCall(t, h, "hook.install", args)
	if err != nil || res["needs_bot_sudo"] != true || len(el.runs) != 1 {
		t.Fatalf("the next request: %v %v (runs %d)", res, err, len(el.runs))
	}
	// five minutes later the prompt is tried again
	now = now.Add(hookNoAgentFor + time.Second)
	el.runErr = nil
	res, err = hookCall(t, h, "hook.install", args)
	if err != nil || res["started"] != true {
		t.Fatalf("after the wait: %v %v", res, err)
	}
	waitIdle(t, h)
	if len(el.runs) != 2 {
		t.Fatalf("runs %d", len(el.runs))
	}
}

// Cancel and timeout keep their results with the new elevators.
func TestCancelAndTimeoutKeepTheirResults(t *testing.T) {
	el := &fakeElev{runErr: ErrAdminCancelled}
	h, _ := noRunHelper(t, el)
	withVersions(t, "", "", "v0.14.0")
	plan, _ := hookCall(t, h, "hook.plan", map[string]any{})
	if _, err := hookCall(t, h, "hook.install", map[string]any{"plan_sha256": plan["plan_sha256"]}); err != nil {
		t.Fatal(err)
	}
	hk := waitIdle(t, h)
	if last, _ := hk["last"].(map[string]any); last["result"] != HookResultCancelled || last["message"] != "The administrator dialog was cancelled." {
		t.Fatalf("cancel: %v", hk)
	}
	h2, _ := noRunHelper(t, &blockElev{})
	h2.hookLimit = 50 * time.Millisecond
	plan2, _ := hookCall(t, h2, "hook.plan", map[string]any{})
	if _, err := hookCall(t, h2, "hook.install", map[string]any{"plan_sha256": plan2["plan_sha256"]}); err != nil {
		t.Fatal(err)
	}
	hk = waitIdle(t, h2)
	if last, _ := hk["last"].(map[string]any); last["result"] != HookResultTimeout || last["message"] != "The administrator dialog timed out; try again." {
		t.Fatalf("timeout: %v", hk)
	}
	_ = version.Version
}

// blockElev waits for its context, the way a prompt nobody answers does.
type blockElev struct{ fakeElev }

func (b *blockElev) Run(ctx context.Context, p InstallPlan) error {
	<-ctx.Done()
	return errors.New("signal: killed")
}

// The result is looked at from outside: after the elevated step ran, InstallHook demands that only an administrator can
// change the installed file (and the plugin), whatever the step said.
func TestInstallHookVerifiesTheResultIsAdminOnly(t *testing.T) {
	src, dst, tools := hashFixture(t)
	oldProbe, oldVerify := ancestorProbe, verifyAdminOnly
	t.Cleanup(func() { ancestorProbe, verifyAdminOnly = oldProbe, oldVerify })
	ancestorProbe = func(string) string { return "" } // the temporary folders are the test user's, not root's
	var verified []string
	verifyAdminOnly = func(p string, _ time.Duration) error { verified = append(verified, p); return nil }
	o := InstallOptions{Src: src, Dst: dst, RequireRoot: true, tools: &tools, Run: copyRunner(nil)}
	if err := InstallHook(context.Background(), &strings.Builder{}, o); err != nil {
		t.Fatal(err)
	}
	if len(verified) != 1 || verified[0] != dst {
		t.Fatalf("verified %v", verified)
	}
	// a verdict of "not admin-only" fails the installation
	verifyAdminOnly = func(p string, _ time.Duration) error {
		return codedf(CodeVerify, "verification failed: %s must be owned by an administrator", p)
	}
	if err := InstallHook(context.Background(), &strings.Builder{}, o); codeOf(err) != CodeVerify {
		t.Fatalf("an unverifiable result: %v", err)
	}
	// without RequireRoot (the tests' own installs) nothing is demanded
	verified = nil
	verifyAdminOnly = func(p string, _ time.Duration) error { verified = append(verified, p); return nil }
	o.RequireRoot = false
	if err := InstallHook(context.Background(), &strings.Builder{}, o); err != nil || len(verified) != 0 {
		t.Fatalf("%v %v", err, verified)
	}
}

// The real check: a file the test user owns is not admin-only.
func TestVerifyAdminOnlyRefusesAFileTheUserCanChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns the file")
	}
	f := filepath.Join(t.TempDir(), "rubi-home")
	os.WriteFile(f, []byte("x"), 0o755)
	if err := verifyAdminOnly(f, time.Second); codeOf(err) != CodeVerify {
		t.Fatalf("%v", err)
	}
}
