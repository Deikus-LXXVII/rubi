package home

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// appRig is a temporary HOME, a bundle with a binary in it, and fakes for launchctl, osascript, the clock and
// the helper's socket.
type appRig struct {
	t            *testing.T
	home         string
	exe          string
	calls        []string
	loaded       bool           // what `launchctl print` answers
	fail         map[string]int // launchctl verb -> how many calls still fail
	dialAt       int            // the helper answers from this poll on (-1: never)
	dials        int
	needsKick    bool   // the helper answers only after a kickstart
	peer         string // the program the helper's socket answers with
	self         string // this app's version
	installed    string // what the installed binary says to `version`
	installedErr error
	versions     int // how many times `version` ran
	hasLog       bool
	block        map[string]bool // programs whose calls block until their context ends
	now          time.Time
	opens        int
	openErr      error
	dialogs      []string
	log          bytes.Buffer
}

func newAppRig(t *testing.T, binary string) *appRig {
	t.Helper()
	r := &appRig{t: t, home: t.TempDir(), fail: map[string]int{}, now: time.Unix(1000, 0), self: "v0.2.0", installed: "v0.1.0", hasLog: true, block: map[string]bool{}}
	r.peer = filepath.Join(r.home, ".rubi-home", "bin", "rubi-home")
	dir := filepath.Join(t.TempDir(), "Rubi Home.app", "Contents", "MacOS")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.exe = filepath.Join(dir, "rubi-home")
	r.setBinary(binary)
	return r
}

func (r *appRig) setBinary(s string) {
	r.t.Helper()
	if err := os.WriteFile(r.exe, []byte(s), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *appRig) run(ctx context.Context, _ string, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if r.block[name] {
		<-ctx.Done()
		return "", ctx.Err()
	}
	switch name {
	case launchctl:
		verb := args[0]
		if verb == "print" {
			if r.loaded {
				return "", nil
			}
			return "", errors.New("exit status 113")
		}
		if r.fail[verb] > 0 {
			r.fail[verb]--
			return "", errors.New("exit status 5")
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected program %s", name)
}

// dialog plays the desktop's dialogs: it records the text and answers OK. block makes it wait for the context, the way a
// dialog nobody answers does.
func (r *appRig) dialog(ctx context.Context, text string, buttons ...string) (string, error) {
	if r.block["/usr/bin/osascript"] {
		<-ctx.Done()
		return "", ctx.Err()
	}
	r.dialogs = append(r.dialogs, text)
	return btnOK, nil
}

func (r *appRig) env() AppEnv {
	return AppEnv{
		Home: r.home, UID: 501, Exe: r.exe, HelperDir: filepath.Join(r.home, "data"),
		Self: r.self, HasLog: r.hasLog,
		Run:     r.run,
		Dialogs: dialogsFunc(r.dialog),
		Version: func(string) (string, error) { r.versions++; return r.installed, r.installedErr },
		Now:     func() time.Time { return r.now },
		Sleep:   func(d time.Duration) { r.now = r.now.Add(d) },
		Dial: func(_ context.Context, socket string) (string, error) {
			r.dials++
			if want := filepath.Join(r.home, "data", "claude.sock"); socket != want {
				r.t.Errorf("dialled %s, want %s", socket, want)
			}
			if r.needsKick {
				if len(r.launchctl("kickstart")) == 0 {
					return "", errors.New("connection refused")
				}
				return r.peer, nil
			}
			if r.dialAt >= 0 && r.dials > r.dialAt {
				return r.peer, nil
			}
			return "", errors.New("connection refused")
		},
		Open:        func(context.Context) error { r.opens++; return r.openErr },
		Logf:        func(f string, a ...any) { fmt.Fprintf(&r.log, f+"\n", a...) },
		CallTimeout: 50 * time.Millisecond, DialogTimeout: 50 * time.Millisecond, OpenTimeout: 50 * time.Millisecond,
	}
}

func (r *appRig) launchctl(verb string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, launchctl+" "+verb) {
			out = append(out, c)
		}
	}
	return out
}

func (r *appRig) bin() string { return filepath.Join(r.home, ".rubi-home", "bin", "rubi-home") }
func (r *appRig) plist() string {
	return filepath.Join(r.home, "Library", "LaunchAgents", "com.rubi-project.home.plist")
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAppFirstOpenInstallsStartsAndShowsTheWindow(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, r.bin()); got != "binary one" {
		t.Fatalf("installed binary %q", got)
	}
	if st, _ := os.Stat(r.bin()); st.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(r.home, ".rubi-home")); st.Mode().Perm() != 0o700 {
		t.Fatalf("~/.rubi-home mode %v", st.Mode())
	}
	if st, err := os.Stat(filepath.Join(r.home, ".rubi-home", "logs")); err != nil || !st.IsDir() {
		t.Fatal("no logs folder")
	}
	want, _ := AgentPlist(r.bin(), filepath.Join(r.home, ".rubi-home", "logs"))
	if mustRead(t, r.plist()) != want {
		t.Fatal("the LaunchAgent is not the one the script writes")
	}
	boot := r.launchctl("bootstrap")
	if len(boot) != 1 || boot[0] != launchctl+" bootstrap gui/501 "+r.plist() {
		t.Fatalf("bootstrap calls %q", boot)
	}
	if len(r.launchctl("bootout")) != 1 {
		t.Fatalf("a new install boots out first (an error is ignored): %q", r.calls)
	}
	if len(r.launchctl("kickstart")) != 0 {
		t.Fatal("kickstart on a new install")
	}
	if r.opens != 1 || len(r.dialogs) != 0 {
		t.Fatalf("window opened %d times, error dialogs %q", r.opens, r.dialogs)
	}
	if es, _ := os.ReadDir(filepath.Dir(r.bin())); len(es) != 1 {
		t.Fatalf("temporary files left in bin: %v", es)
	}
}

func TestAppOpenedAgainWithTheSameBinaryOnlyShowsTheWindow(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(5000, 0)
	if err := os.Chtimes(r.bin(), old, old); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded = nil, true
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(r.bin()); !st.ModTime().Equal(old) {
		t.Fatal("an equal binary was copied again")
	}
	for _, verb := range []string{"bootstrap", "bootout", "kickstart"} {
		if len(r.launchctl(verb)) != 0 {
			t.Fatalf("%s on an unchanged, loaded agent: %q", verb, r.calls)
		}
	}
	if r.opens != 2 {
		t.Fatalf("the window must show every time: %d", r.opens)
	}
}

func TestAppOpenedWithANewerBinaryCopiesAndRestarts(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded = nil, true
	r.setBinary("binary two")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, r.bin()) != "binary two" {
		t.Fatal("the new binary was not copied")
	}
	k := r.launchctl("kickstart")
	if len(k) != 1 || k[0] != launchctl+" kickstart -k gui/501/com.rubi-project.home" {
		t.Fatalf("kickstart calls %q", k)
	}
	if len(r.launchctl("bootstrap")) != 0 {
		t.Fatalf("bootstrap on a loaded agent: %q", r.calls)
	}
	if r.opens != 2 {
		t.Fatal("no window")
	}
}

func TestAppNewerBinaryButAgentNotLoadedBootstraps(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded = nil, false // the user logged the agent out
	r.setBinary("binary two")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if len(r.launchctl("bootstrap")) != 1 || len(r.launchctl("kickstart")) != 0 {
		t.Fatalf("calls %q", r.calls)
	}
}

func TestAppChangedAgentFileIsReloaded(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.plist(), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded = nil, true
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if len(r.launchctl("bootout")) != 1 || len(r.launchctl("bootstrap")) != 1 {
		t.Fatalf("a loaded agent with a new file must be reloaded: %q", r.calls)
	}
	want, _ := AgentPlist(r.bin(), filepath.Join(r.home, ".rubi-home", "logs"))
	if mustRead(t, r.plist()) != want {
		t.Fatal("the file was not rewritten")
	}
}

func TestAppBootstrapRetriesOnceThenReportsTheFailure(t *testing.T) {
	r := newAppRig(t, "binary one")
	r.fail["bootstrap"] = 1
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatalf("one failure is retried: %v", err)
	}
	if len(r.launchctl("bootstrap")) != 2 || len(r.launchctl("enable")) != 1 || r.launchctl("enable")[0] != launchctl+" enable gui/501/com.rubi-project.home" {
		t.Fatalf("a failed bootstrap enables the agent once and tries again: %q", r.calls)
	}

	r = newAppRig(t, "binary one")
	r.fail["bootstrap"] = 2
	if err := RunApp(context.Background(), r.env()); err == nil {
		t.Fatal("two failures must fail")
	}
	if r.opens != 0 || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "could not start itself") {
		t.Fatalf("opens %d, dialogs %q", r.opens, r.dialogs)
	}
}

func TestAppHelperNeverComesUpShowsAClearDialog(t *testing.T) {
	r := newAppRig(t, "binary one")
	r.dialAt = -1
	start := r.now
	err := RunApp(context.Background(), r.env())
	if err == nil {
		t.Fatal("no error")
	}
	if waited := r.now.Sub(start); waited < 10*time.Second || waited > 11*time.Second {
		t.Fatalf("waited %v, want about 10 s", waited)
	}
	if r.opens != 0 {
		t.Fatal("the window opened without a helper")
	}
	if len(r.launchctl("kickstart")) != 0 {
		t.Fatal("an agent we just loaded is not kickstarted")
	}
	if len(r.dialogs) != 1 {
		t.Fatalf("dialogs %q", r.dialogs)
	}
	d := r.dialogs[0]
	for _, want := range []string{"did not start", "Open it again", "rubi-home-app.log"} {
		if !strings.Contains(d, want) {
			t.Errorf("dialog %q lacks %q", d, want)
		}
	}
	for _, bad := range []string{"launchctl", "Terminal", "sudo", "run ", "connection refused", r.home} {
		if strings.Contains(d, bad) {
			t.Errorf("dialog %q holds %q", d, bad)
		}
	}
	if !strings.Contains(r.log.String(), "connection refused") {
		t.Fatalf("the log lacks the detail: %q", r.log.String())
	}
}

func TestAppHelperThatComesUpLateIsAwaited(t *testing.T) {
	r := newAppRig(t, "binary one")
	r.dialAt = 12 // about three seconds
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if r.opens != 1 || r.dials != 13 {
		t.Fatalf("opens %d dials %d", r.opens, r.dials)
	}
}

func TestAppWindowFailureAndCopyFailureAreShown(t *testing.T) {
	r := newAppRig(t, "binary one")
	r.openErr = errors.New("osascript: boom")
	if err := RunApp(context.Background(), r.env()); err == nil || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "could not show its window") {
		t.Fatalf("err %v dialogs %q", err, r.dialogs)
	}

	r = newAppRig(t, "binary one")
	if err := os.WriteFile(filepath.Join(r.home, ".rubi-home"), []byte("a file, not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RunApp(context.Background(), r.env()); err == nil || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "folder isn't safe") {
		t.Fatalf("err %v dialogs %q", err, r.dialogs)
	}
	if len(r.launchctl("bootstrap")) != 0 {
		t.Fatal("the agent was loaded after a failed copy")
	}
}

func TestAppTightensTheFolderOfAnEarlierInstall(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := os.MkdirAll(filepath.Join(r.home, ".rubi-home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(r.home, ".rubi-home")); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode())
	}
}

// The script installer and the app load the same agent: the heredoc in panel/install-home.sh, with its two
// variables filled in, is what AgentPlist writes.
func TestAgentPlistIsTheOneInstallHomeShWrites(t *testing.T) {
	sh := mustRead(t, filepath.Join("..", "..", "panel", "install-home.sh"))
	_, rest, ok := strings.Cut(sh, "<<PLIST\n")
	if !ok {
		t.Fatal("no PLIST heredoc in install-home.sh")
	}
	body, _, ok := strings.Cut(rest, "\nPLIST\n")
	if !ok {
		t.Fatal("heredoc not closed")
	}
	bin, dir := "/Users/x/.rubi-home/bin/rubi-home", "/Users/x/.rubi-home"
	got := strings.NewReplacer("$bin", bin, "$DIR", dir).Replace(body) + "\n"
	if strings.Contains(got, "$") {
		t.Fatalf("the script's heredoc has a variable this test does not know:\n%s", got)
	}
	want, err := AgentPlist(bin, dir+"/logs")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("script and app disagree.\nscript:\n%s\napp:\n%s", got, want)
	}
}

func TestAgentPlistEscapesAwkwardPaths(t *testing.T) {
	p, err := AgentPlist("/Users/a&b/<c>/rubi-home", "/Users/a&b/<c>/logs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, "/Users/a&amp;b/&lt;c&gt;/rubi-home") {
		t.Fatalf("not escaped:\n%s", p)
	}
	d := xml.NewDecoder(strings.NewReader(p))
	d.Strict = true
	d.Entity = xml.HTMLEntity
	for {
		if _, err := d.Token(); err != nil {
			if err.Error() != "EOF" {
				t.Fatalf("not XML: %v", err)
			}
			break
		}
	}
}

// bundleAt makes <root>/<app>/Contents/{Info.plist,MacOS/rubi-home} and returns the executable.
func bundleAt(t *testing.T, root, app, plist string) string {
	t.Helper()
	exe := filepath.Join(root, app, "Contents", "MacOS", "rubi-home")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if plist != "" {
		if err := os.WriteFile(filepath.Join(root, app, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return exe
}

func infoPlist(id string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleName</key><string>Rubi Home</string>` +
		`<key>CFBundleIdentifier</key><string>` + id + `</string></dict></plist>`
}

func TestIsAppLaunch(t *testing.T) {
	root := t.TempDir()
	ours := infoPlist("com.rubi-project.home")
	in := bundleAt(t, root, "Rubi Home.app", ours)
	moved := bundleAt(t, root, "Elsewhere/Downloads/Rubi Home.app", ours)
	foreign := bundleAt(t, root, "Other.app", infoPlist("com.example.other"))
	noPlist := bundleAt(t, root, "NoPlist.app", "")
	junk := bundleAt(t, root, "Junk.app", "not xml at all")
	idAsName := bundleAt(t, root, "Name.app", `<plist><dict><key>CFBundleName</key><string>com.rubi-project.home</string></dict></plist>`)
	notApp := bundleAt(t, root, "Plain", ours)
	resources := filepath.Join(root, "Rubi Home.app", "Contents", "Resources", "rubi-home")
	cases := []struct {
		name string
		args []string
		exe  string
		want bool
	}{
		{"no arguments", nil, in, true},
		{"old macOS psn", []string{"-psn_0_12345"}, in, true},
		{"moved app", nil, moved, true},
		{"bare binary", nil, "/Users/x/.rubi-home/bin/rubi-home", false},
		{"an argument", []string{"run"}, in, false},
		{"psn and an argument", []string{"-psn_0_1", "run"}, in, false},
		{"status", []string{"status"}, in, false},
		{"another app's identifier", nil, foreign, false},
		{"no Info.plist", nil, noPlist, false},
		{"Info.plist that is not a plist", nil, junk, false},
		{"the identifier under another key", nil, idAsName, false},
		{"not named .app", nil, notApp, false},
		{"Resources, not MacOS", nil, resources, false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		if got := IsAppLaunch(c.args, c.exe); got != c.want {
			t.Errorf("%s: IsAppLaunch(%q, %q) = %v, want %v", c.name, c.args, c.exe, got, c.want)
		}
	}
}

func TestDialSocketTellsALiveHelperFromALeftoverFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "claude.sock")
	if _, err := dialSocket(context.Background(), sock); err == nil {
		t.Fatal("no socket, yet it answered")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := dialSocket(context.Background(), sock)
	if err != nil {
		t.Fatalf("a listening socket: %v", err)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		self, _ := os.Executable()
		if !sameFile(exe, self) { // the listener is this very process
			t.Fatalf("the peer's program is %q, want %q", exe, self)
		}
	}
	ln.Close() // a closed Go listener removes the file; a crash leaves it, so put one back
	if f, err := os.Create(sock); err == nil {
		f.Close()
	}
	if _, err := dialSocket(context.Background(), sock); err == nil {
		t.Fatal("a leftover file counts as a running helper")
	}
}

func TestOpenAppLogIsPrivate(t *testing.T) {
	home := t.TempDir()
	f := OpenAppLog(home)
	if f == nil {
		t.Fatal("no log")
	}
	f.Close()
	if st, _ := os.Stat(f.Name()); st.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(home, ".rubi-home")); st.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", st.Mode())
	}
}

// ---- a downgrade, a dead agent, timeouts, unsafe folders ----

func TestAppKeepsANewerInstalledBinary(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded = nil, true
	r.setBinary("an older download")
	r.self, r.installed = "v0.1.0", "v0.2.0"
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, r.bin()) != "binary one" {
		t.Fatal("an older app replaced a newer Rubi Home")
	}
	if len(r.launchctl("kickstart")) != 0 || len(r.launchctl("bootstrap")) != 0 {
		t.Fatalf("the helper was restarted: %q", r.calls)
	}
	if len(r.dialogs) != 1 || r.dialogs[0] != "A newer Rubi Home (v0.2.0) is already installed; this app is v0.1.0. Keep the newer one." {
		t.Fatalf("dialogs %q", r.dialogs)
	}
	if r.opens != 2 {
		t.Fatal("the window must still show, for the installed helper")
	}
}

func TestAppUpdatesAnOlderOrUnreadableOrEqualVersion(t *testing.T) {
	for name, c := range map[string]struct {
		installed string
		err       error
	}{
		"older":      {"v0.1.0", nil},
		"unreadable": {"", errors.New("it didn't answer `version`")},
		"same tag":   {"v0.2.0", nil}, // a rebuild of the same release
		"older rc":   {"v0.2.0-rc.1", nil},
	} {
		r := newAppRig(t, "binary one")
		if err := RunApp(context.Background(), r.env()); err != nil {
			t.Fatal(err)
		}
		r.calls, r.loaded = nil, true
		r.setBinary("binary two")
		r.installed, r.installedErr = c.installed, c.err
		if err := RunApp(context.Background(), r.env()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if mustRead(t, r.bin()) != "binary two" || len(r.launchctl("kickstart")) != 1 || len(r.dialogs) != 0 {
			t.Errorf("%s: not updated (%q, %q)", name, r.calls, r.dialogs)
		}
	}
}

func TestKeepInstalledRules(t *testing.T) {
	bad := errors.New("x")
	for _, c := range []struct {
		installed, self string
		readErr         error
		keep            bool
	}{
		{"v1.0.0", "v1.1.0", nil, false},
		{"v1.1.0", "v1.0.0", nil, true},
		{"v1.0.0", "v1.0.0", nil, false},
		{"v1.0.0-rc.1", "v1.0.0", nil, false},
		{"v1.0.0", "v1.0.0-rc.1", nil, true},
		{"v0.9.0", "v0.10.0", nil, false}, // numbers, not text
		{"dev", "dev", nil, false},        // two development builds
		{"v1.0.0", "dev", nil, true},      // a development build does not replace a release
		{"dev", "v1.0.0", nil, true},      // a release does not replace someone's development build
		{"v9.0.0", "v1.0.0", bad, false},  // unreadable: replaced
	} {
		if keep, _ := keepInstalled(c.installed, c.readErr, c.self); keep != c.keep {
			t.Errorf("installed %q self %q err %v: keep = %v, want %v", c.installed, c.self, c.readErr, keep, c.keep)
		}
	}
}

func TestAppKeepsADevelopmentBuildWithItsOwnDialog(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.loaded = true
	r.setBinary("binary two")
	r.installed, r.self = "dev", "v1.0.0"
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if mustRead(t, r.bin()) != "binary one" || len(r.dialogs) != 1 || strings.Contains(r.dialogs[0], "newer") {
		t.Fatalf("binary %q dialogs %q", mustRead(t, r.bin()), r.dialogs)
	}
}

func TestAppRestartsALoadedAgentThatDoesNotAnswer(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded, r.needsKick = nil, true, true
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatalf("a restart should have fixed it: %v", err)
	}
	if k := r.launchctl("kickstart"); len(k) != 1 || k[0] != launchctl+" kickstart -k gui/501/com.rubi-project.home" {
		t.Fatalf("kickstart calls %q", k)
	}
	if r.opens != 2 || len(r.dialogs) != 0 {
		t.Fatalf("opens %d dialogs %q", r.opens, r.dialogs)
	}

	// Still dead after the one restart: an error, and not a second restart.
	r.calls, r.needsKick, r.dialAt, r.opens = nil, false, -1, 0
	if err := RunApp(context.Background(), r.env()); err == nil {
		t.Fatal("a dead helper was reported as up")
	}
	if len(r.launchctl("kickstart")) != 1 || len(r.dialogs) != 1 || r.opens != 0 {
		t.Fatalf("calls %q dialogs %q opens %d", r.calls, r.dialogs, r.opens)
	}
}

func TestAppDoesNotRestartTwiceWhenItJustRestarted(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	r.calls, r.loaded, r.dialAt, r.dials = nil, true, -1, 0
	r.setBinary("binary two") // the update kickstarts; that is the one restart
	if err := RunApp(context.Background(), r.env()); err == nil {
		t.Fatal("no error")
	}
	if len(r.launchctl("kickstart")) != 1 {
		t.Fatalf("calls %q", r.calls)
	}
}

func TestAppSaysWhenAnotherProgramAnswers(t *testing.T) {
	for peer, want := range map[string]string{
		"/opt/other/rubi-home": "another program",
		"":                     "could not be identified",
	} {
		r := newAppRig(t, "binary one")
		r.peer = peer
		if err := RunApp(context.Background(), r.env()); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.log.String(), want) || r.opens != 1 {
			t.Errorf("peer %q: log %q opens %d", peer, r.log.String(), r.opens)
		}
	}
	r := newAppRig(t, "binary one")
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.log.String(), "another program") || strings.Contains(r.log.String(), "identified") {
		t.Fatalf("a warning for the right program: %q", r.log.String())
	}
}

func TestAppCallsAndDialogsHaveTimeouts(t *testing.T) {
	r := newAppRig(t, "binary one")
	r.block[launchctl] = true
	start := time.Now()
	if err := RunApp(context.Background(), r.env()); err == nil {
		t.Fatal("a launchctl that never answers was fine")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "could not start itself") {
		t.Fatalf("dialogs %q", r.dialogs)
	}

	r = newAppRig(t, "binary one")
	r.block["/usr/bin/osascript"] = true // even the error dialog must not hold the app forever
	r.openErr = errors.New("boom")
	start = time.Now()
	if err := RunApp(context.Background(), r.env()); err == nil {
		t.Fatal("no error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the error dialog held the app for %v", time.Since(start))
	}
}

func TestAppPairingWindowIsCappedButIsNoFailure(t *testing.T) {
	r := newAppRig(t, "binary one")
	env := r.env()
	env.Open = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	start := time.Now()
	if err := RunApp(context.Background(), env); err != nil {
		t.Fatalf("a window left open is not an error: %v", err)
	}
	if time.Since(start) > 5*time.Second || len(r.dialogs) != 0 {
		t.Fatalf("took %v, dialogs %q", time.Since(start), r.dialogs)
	}
}

func TestAppRefusesUnsafeFolders(t *testing.T) {
	cases := map[string]func(r *appRig){
		"~/.rubi-home is a symlink": func(r *appRig) {
			elsewhere := t.TempDir()
			if err := os.Symlink(elsewhere, filepath.Join(r.home, ".rubi-home")); err != nil {
				t.Fatal(err)
			}
		},
		"bin is a symlink": func(r *appRig) {
			if err := os.MkdirAll(filepath.Join(r.home, ".rubi-home"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(r.home, ".rubi-home", "bin")); err != nil {
				t.Fatal(err)
			}
		},
		"logs is a symlink": func(r *appRig) {
			if err := os.MkdirAll(filepath.Join(r.home, ".rubi-home"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(r.home, ".rubi-home", "logs")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		r := newAppRig(t, "binary one")
		setup(r)
		if err := RunApp(context.Background(), r.env()); err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "folder isn't safe to install into") || len(r.launchctl("bootstrap")) != 0 || r.opens != 0 {
			t.Errorf("%s: dialogs %q calls %q", name, r.dialogs, r.calls)
		}
	}

	// A folder that someone else owns: the user id the app runs as is not the owner.
	r := newAppRig(t, "binary one")
	env := r.env()
	env.UID = os.Getuid() + 1
	if err := RunApp(context.Background(), env); err == nil || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "isn't safe") {
		t.Fatalf("err %v dialogs %q", err, r.dialogs)
	}
}

func TestAppTakesAwayWriteAccessFromOthers(t *testing.T) {
	r := newAppRig(t, "binary one")
	bin := filepath.Join(r.home, ".rubi-home", "bin")
	if err := os.MkdirAll(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(bin); st.Mode().Perm()&0o022 != 0 {
		t.Fatalf("bin is %v", st.Mode())
	}
}

func TestAppReplacesASymlinkAtTheBinaryNeverTreatingItAsEqual(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := os.MkdirAll(filepath.Join(r.home, ".rubi-home", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "same-content")
	if err := os.WriteFile(target, []byte("binary one"), 0o755); err != nil { // equal bytes behind the link
		t.Fatal(err)
	}
	if err := os.Symlink(target, r.bin()); err != nil {
		t.Fatal(err)
	}
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(r.bin()); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the symlink is still there: %v %v", fi, err)
	}
	if mustRead(t, target) != "binary one" || r.versions != 0 {
		t.Fatalf("the link's target was touched or run (version ran %d times)", r.versions)
	}
	if !strings.Contains(r.log.String(), "not a regular file") {
		t.Fatalf("log %q", r.log.String())
	}
}

func TestAppReplacesASymlinkAtTheAgentFile(t *testing.T) {
	r := newAppRig(t, "binary one")
	if err := os.MkdirAll(filepath.Dir(r.plist()), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, r.plist()); err != nil {
		t.Fatal(err)
	}
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(r.plist()); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the agent file is still a link: %v %v", fi, err)
	}
	if mustRead(t, victim) != "keep me" {
		t.Fatal("the link's target was written")
	}
	if !strings.Contains(r.log.String(), "not a regular file") {
		t.Fatalf("log %q", r.log.String())
	}
}

func TestAppDialogPointsToTheLogOnlyWhenThereIsOne(t *testing.T) {
	for _, has := range []bool{true, false} {
		r := newAppRig(t, "binary one")
		r.hasLog = has
		r.dialAt = -1
		if err := RunApp(context.Background(), r.env()); err == nil {
			t.Fatal("no error")
		}
		if got := strings.Contains(r.dialogs[0], "rubi-home-app.log"); got != has {
			t.Errorf("log %v: dialog %q", has, r.dialogs[0])
		}
	}
}

func TestUserHomeIsAbsolute(t *testing.T) {
	t.Setenv("HOME", "relative/home")
	h, err := UserHome()
	if err == nil && !filepath.IsAbs(h) {
		t.Fatalf("a relative home: %q", h)
	}
	t.Setenv("HOME", "")
	if h, err := UserHome(); err == nil && !filepath.IsAbs(h) {
		t.Fatalf("a relative home: %q", h)
	}
	if _, err := UserHome(); err != nil && !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error %v", err)
	}
}
