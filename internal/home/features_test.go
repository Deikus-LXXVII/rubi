package home

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// fakeDesktop is a Desktop of one system with a recording runner: which programs it has, and what the registry or xdg-mime
// answer for the claude link.
type fakeDesktop struct {
	ran        [][]string
	registered bool
}

func (f *fakeDesktop) env(goos string, has ...string) DesktopEnv {
	return DesktopEnv{
		Run: func(_ context.Context, _ string, name string, args ...string) (string, error) {
			f.ran = append(f.ran, append([]string{name}, args...))
			if !f.registered {
				if strings.HasSuffix(name, "xdg-mime") {
					return "\n", nil // nothing registered: xdg-mime prints nothing
				}
				return "", &exec.ExitError{} // reg.exe: not found
			}
			if strings.HasSuffix(name, "xdg-mime") {
				return "claude.desktop\n", nil
			}
			return "", nil
		},
		LookPath: func(n string) (string, error) {
			for _, h := range has {
				if n == h {
					return n, nil
				}
			}
			return "", exec.ErrNotFound
		},
		Getenv:    func(k string) string { return map[string]string{"DISPLAY": ":0"}[k] },
		SystemDir: `C:\Windows\System32`,
	}
}

var (
	linuxTools   = []string{"xdg-open", "xdg-mime"}
	windowsTools = []string{`C:\Windows\System32\rundll32.exe`, `C:\Windows\System32\reg.exe`}
)

func avail(f map[string]Feature) string {
	var on []string
	for _, n := range featureOrder {
		if f[n].Available {
			on = append(on, n)
		}
	}
	return strings.Join(on, ",")
}

// What each system offers, feature by feature. A Mac has everything. Linux and Windows have no Shortcuts, no Apple Home, and no
// agents (there is no virtual machine backend there yet); the Claude desktop link is there only when a program is registered
// for it.
func TestFeaturesPerSystem(t *testing.T) {
	ctx := context.Background()
	const all = "shortcuts,apple_home,claude-start,agent,t3"
	for name, c := range map[string]struct {
		goos       string
		desktop    func(f *fakeDesktop) Desktop
		registered bool
		want       string
		reasons    map[string]string
	}{
		"macOS": {"darwin", func(*fakeDesktop) Desktop { return Desktop{} }, false, all, nil},
		"Linux with the app registered": {"linux", func(f *fakeDesktop) Desktop { return xdgDesktop(f.env("linux", linuxTools...)) }, true, "claude-start",
			map[string]string{"shortcuts": "it needs macOS", "apple_home": "it needs macOS", "agent": "agents don't run on Linux yet", "t3": "agents don't run on Linux yet"}},
		"Linux without it": {"linux", func(f *fakeDesktop) Desktop { return xdgDesktop(f.env("linux", linuxTools...)) }, false, "",
			map[string]string{"claude-start": "the Claude desktop app isn't installed here"}},
		"Linux with no desktop": {"linux", func(f *fakeDesktop) Desktop { return Desktop{} }, false, "",
			map[string]string{"claude-start": "this computer has no desktop to open a link on"}},
		"Windows with the app registered": {"windows", func(f *fakeDesktop) Desktop { return winDesktop(f.env("windows", windowsTools...)) }, true, "claude-start",
			map[string]string{"shortcuts": "it needs macOS", "agent": "agents don't run on Windows yet", "t3": "agents don't run on Windows yet"}},
		"Windows without it": {"windows", func(f *fakeDesktop) Desktop { return winDesktop(f.env("windows", windowsTools...)) }, false, "",
			map[string]string{"claude-start": "the Claude desktop app isn't installed here"}},
		"another system": {"freebsd", func(f *fakeDesktop) Desktop { return Desktop{} }, false, "",
			map[string]string{"agent": "agents don't run on this system yet"}},
	} {
		f := &fakeDesktop{registered: c.registered}
		got := ComputeFeatures(ctx, FeatureEnv{GOOS: c.goos, Desktop: c.desktop(f)})
		if avail(got) != c.want {
			t.Errorf("%s: available %q, want %q", name, avail(got), c.want)
		}
		for n, why := range c.reasons {
			if got[n].Available || got[n].Reason != why {
				t.Errorf("%s: %s = %+v, want reason %q", name, n, got[n], why)
			}
		}
		for _, n := range featureOrder {
			if got[n].Available != (got[n].Reason == "") {
				t.Errorf("%s: %s has available=%v and reason %q", name, n, got[n].Available, got[n].Reason)
			}
		}
		if len(got) != len(featureOrder) {
			t.Errorf("%s: %d features", name, len(got))
		}
	}
}

// The handler checks run the right program with fixed arguments and nothing else: xdg-mime for the Linux link, reg.exe for the
// Windows one, and the scheme can't smuggle anything in.
func TestSchemeHandlerChecksUseFixedArguments(t *testing.T) {
	f := &fakeDesktop{registered: true}
	lx := xdgDesktop(f.env("linux", linuxTools...))
	if !lx.Scheme.Registered(context.Background(), "claude") {
		t.Fatal("linux: registered, answered no")
	}
	wx := winDesktop(f.env("windows", windowsTools...))
	if !wx.Scheme.Registered(context.Background(), "claude") {
		t.Fatal("windows: registered, answered no")
	}
	want := [][]string{{"xdg-mime", "query", "default", "x-scheme-handler/claude"},
		{`C:\Windows\System32\reg.exe`, "query", `HKCR\claude`, "/v", "URL Protocol"}}
	if len(f.ran) != 2 || strings.Join(f.ran[0], "|") != strings.Join(want[0], "|") || strings.Join(f.ran[1], "|") != strings.Join(want[1], "|") {
		t.Fatalf("ran %q, want %q", f.ran, want)
	}
	f.ran = nil
	for _, bad := range []string{"", "Claude", "claude;rm", `claude\x`, "claude/../x", "a b", strings.Repeat("a", 40)} {
		if lx.Scheme.Registered(context.Background(), bad) || wx.Scheme.Registered(context.Background(), bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
	if len(f.ran) != 0 {
		t.Fatalf("a bad scheme still ran %q", f.ran)
	}
	// a tool that is missing leaves no handler check at all, which is "not available"
	if d := xdgDesktop(f.env("linux", "xdg-open")); d.Scheme != nil {
		t.Error("xdg-mime missing, but a handler check exists")
	}
	if d := winDesktop(f.env("windows", `C:\Windows\System32\rundll32.exe`)); d.Scheme != nil {
		t.Error("reg.exe missing, but a handler check exists")
	}
}

// Run on the system the tests are running on, with nothing faked but the desktop: the real build gives the real answer.
func TestFeaturesOfThisSystem(t *testing.T) {
	got := ComputeFeatures(context.Background(), FeatureEnv{GOOS: runtime.GOOS})
	mac := runtime.GOOS == "darwin"
	for _, n := range []string{FeatureShortcuts, FeatureAppleHome, FeatureAgent, FeatureT3} {
		if got[n].Available != mac {
			t.Errorf("%s on %s: %+v", n, runtime.GOOS, got[n])
		}
	}
	if got[FeatureClaudeStart].Available != mac {
		t.Errorf("claude-start with no desktop on %s: %+v", runtime.GOOS, got[FeatureClaudeStart])
	}
	if !mac {
		// the build itself does not run the Shortcuts command
		if _, err := runShortcuts(context.Background(), "list"); !isNotAvailable(err) {
			t.Errorf("runShortcuts off macOS: %v", err)
		}
	}
}

func isNotAvailable(err error) bool {
	var ce *homeproto.CodedError
	return errors.As(err, &ce) && ce.Code == homeproto.CodeNotAvailableHere
}

// handleOps are the operations of Handle, read from its source: every string of a case clause.
func handleOps(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "home.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Handle" {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if cc, ok := n.(*ast.CaseClause); ok {
				for _, e := range cc.List {
					if bl, ok := e.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						s, _ := strconv.Unquote(bl.Value)
						ops = append(ops, s)
					}
				}
			}
			return true
		})
	}
	sort.Strings(ops)
	return ops
}

func helperAs(t *testing.T, env FeatureEnv) *Helper {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.featureEnv = func() FeatureEnv { return env }
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: tokenHash("tok")}} }); err != nil {
		t.Fatal(err)
	}
	return h
}

// Every operation Home answers is either tied to a feature or not, and where the feature is not available it answers the same
// not_available_here error with the same sentence: no crash, no program's own message, nothing started. Where it is available
// the gate lets it through.
func TestEveryOpOfAnUnavailableFeatureAnswersNotAvailableHere(t *testing.T) {
	ops := handleOps(t)
	tied := map[string][]string{} // feature -> its ops
	for _, op := range ops {
		if f := opFeature(op); f != "" {
			tied[f] = append(tied[f], op)
		}
	}
	for _, f := range featureOrder {
		if f == FeatureAppleHome {
			continue // reached only through Shortcuts: it has no operations of its own
		}
		if len(tied[f]) == 0 {
			t.Errorf("feature %s has no operation in Handle: the table opFeatures is out of date", f)
		}
	}
	if len(tied[FeatureAgent]) < 10 || len(tied[FeatureT3]) < 10 || len(tied[FeatureShortcuts]) != 2 {
		t.Fatalf("operations found: %v", tied)
	}
	for _, op := range []string{"hello", "hue.discover", "hook.plan", "hook.install", "claude.next", "claude-sessions.list", "fs.list", "unpair", "library.key"} {
		if opFeature(op) != "" {
			t.Errorf("%s is tied to feature %s", op, opFeature(op))
		}
	}
	for name, env := range map[string]FeatureEnv{
		"Linux":   {GOOS: "linux", Desktop: xdgDesktop((&fakeDesktop{}).env("linux", linuxTools...))},
		"Windows": {GOOS: "windows", Desktop: winDesktop((&fakeDesktop{}).env("windows", windowsTools...))},
	} {
		h := helperAs(t, env)
		for _, f := range featureOrder {
			feat := h.Features()[f]
			for _, op := range tied[f] {
				_, err := h.Handle(context.Background(), homeproto.Envelope{Op: op, Token: "tok", Args: json.RawMessage(`{}`)})
				if feat.Available {
					if isNotAvailable(err) {
						t.Errorf("%s: %s is available and was refused", name, op)
					}
					continue
				}
				var ce *homeproto.CodedError
				if !errors.As(err, &ce) || ce.Code != "not_available_here" || ce.Message != NotAvailableMessage(feat) ||
					!strings.HasPrefix(ce.Message, "That isn't available on this computer (") {
					t.Errorf("%s: %s answered %v", name, op, err)
				}
			}
		}
		// what is not tied to a feature is not refused
		for _, op := range []string{"hue.discover", "fs.list", "claude.next"} {
			if err := h.gate(op); err != nil {
				t.Errorf("%s: %s refused: %v", name, op, err)
			}
		}
	}
}

// hello says what is available, lists in `can` only that, and carries every feature with its reason; on a Mac it lists what it
// always did.
func TestHelloListsOnlyWhatIsAvailable(t *testing.T) {
	mac := helperAs(t, FeatureEnv{GOOS: "darwin"})
	hello := mac.hello()
	want := []string{"hue", "shortcuts", "claude", "claude-sessions", "claude-start", "claude-agent", "agent-setup", "t3-agent", "t3-setup"}
	if got := hello["can"].([]string); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("macOS can: %v", got)
	}
	win := helperAs(t, FeatureEnv{GOOS: "windows", Desktop: winDesktop((&fakeDesktop{registered: true}).env("windows", windowsTools...))})
	hello = win.hello()
	if got := hello["can"].([]string); strings.Join(got, ",") != "hue,claude,claude-sessions,claude-start" {
		t.Fatalf("Windows can: %v", got)
	}
	raw, _ := json.Marshal(hello["features"])
	var feats map[string]struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &feats); err != nil || feats["agent"].Available || feats["agent"].Reason != "agents don't run on Windows yet" ||
		!feats["claude-start"].Available || feats["shortcuts"].Reason != "it needs macOS" {
		t.Fatalf("features: %s", raw)
	}
	// through the wire format the code survives: the client gets the typed error back
	_, err := win.Handle(context.Background(), homeproto.Envelope{Op: "shortcuts.list", Token: "tok"})
	if err == nil || err.Error() != "That isn't available on this computer (it needs macOS)." {
		t.Fatalf("shortcuts.list: %v", err)
	}
}

// The answer is kept for a while, not worked out for every call, and picks up a change after that (the user installed the app).
func TestFeaturesAreCachedAndRefreshed(t *testing.T) {
	calls := 0
	registered := false
	h := helperAs(t, FeatureEnv{})
	h.featureEnv = func() FeatureEnv {
		calls++
		f := &fakeDesktop{registered: registered}
		return FeatureEnv{GOOS: "linux", Desktop: xdgDesktop(f.env("linux", linuxTools...))}
	}
	h.Features()
	h.Features()
	if calls != 1 || h.Features()[FeatureClaudeStart].Available {
		t.Fatalf("%d calls", calls)
	}
	registered = true
	old := featureTTL
	featureTTL = 0
	defer func() { featureTTL = old }()
	if !h.Features()[FeatureClaudeStart].Available || calls != 2 {
		t.Fatalf("after the app was installed: %d calls", calls)
	}
}
