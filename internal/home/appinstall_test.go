package home

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// RunApp on every system, with a fake Autostart: what the app does around the start at login. The launchd form is tested
// call for call in appinstall_darwin_test.go; systemd and the Scheduled Task in autostart_*_test.go.

// fakeAutostart records what the app asked of the start at login and answers from its fields.
type fakeAutostart struct {
	mu         sync.Mutex
	installs   []AutostartSpec
	restarts   int
	result     AutostartResult
	installErr error
	restartErr error
}

func (f *fakeAutostart) Install(_ context.Context, s AutostartSpec) (AutostartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs = append(f.installs, s)
	return f.result, f.installErr
}
func (f *fakeAutostart) Restart(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts++
	return f.restartErr
}
func (f *fakeAutostart) Remove(context.Context) error { return nil }
func (f *fakeAutostart) Loaded(context.Context) bool  { return false }

// flowRig is a home folder, a binary to install, and fakes for the rest.
type flowRig struct {
	t       *testing.T
	home    string
	exe     string
	as      *fakeAutostart
	dialogs []string
	opens   int
	dials   int
	dialAt  int // the dial that finds the helper (-1: never)
	now     time.Time
}

func newFlowRig(t *testing.T) *flowRig {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	exe := filepath.Join(home, "Downloads", "rubi-home")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("binary one"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &flowRig{t: t, home: home, exe: exe, as: &fakeAutostart{}, dialAt: 0, now: time.Unix(1_700_000_000, 0)}
}

func (r *flowRig) env() AppEnv {
	return AppEnv{
		Home: r.home, UID: os.Getuid(), Exe: r.exe, HelperDir: filepath.Join(r.home, "data"), Self: "v1.0.0",
		Autostart: r.as,
		Dialogs: dialogsFunc(func(_ context.Context, text string, _ ...string) (string, error) {
			r.dialogs = append(r.dialogs, text)
			return btnOK, nil
		}),
		Version: func(string) (string, error) { return "v1.0.0", nil },
		Now:     func() time.Time { return r.now },
		Sleep:   func(d time.Duration) { r.now = r.now.Add(d) },
		Dial: func(context.Context, string) (string, error) {
			r.dials++
			if r.dialAt >= 0 && r.dials > r.dialAt {
				return "", nil
			}
			return "", errors.New("connection refused")
		},
		Open: func(context.Context) error { r.opens++; return nil },
	}
}

func TestAppFlowInstallsTheBinaryStartsItAtLoginAndShowsTheWindow(t *testing.T) {
	r := newFlowRig(t)
	r.as.result = AutostartResult{Restarted: true}
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	dir := hostpaths.HelperHome(r.home)
	installed := filepath.Join(dir, "bin", "rubi-home")
	if b, err := os.ReadFile(installed); err != nil || string(b) != "binary one" {
		t.Fatalf("installed binary: %q %v", b, err)
	}
	want := AutostartSpec{Bin: installed, Logs: filepath.Join(dir, "logs"), BinChanged: true}
	if len(r.as.installs) != 1 || r.as.installs[0] != want || r.opens != 1 || len(r.dialogs) != 0 || r.as.restarts != 0 {
		t.Fatalf("installs %+v opens %d dialogs %q restarts %d", r.as.installs, r.opens, r.dialogs, r.as.restarts)
	}
	// opened again with the same binary: the binary is not replaced
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if len(r.as.installs) != 2 || r.as.installs[1].BinChanged || r.opens != 2 {
		t.Fatalf("second open: %+v opens %d", r.as.installs, r.opens)
	}
}

func TestAppFlowTellsAFailureToWriteFromAFailureToStart(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"write":               {errors.Join(errAutostartWrite, errors.New("disk full")), "could not set itself up to start when you log in"},
		"load":                {errors.New("launchctl said no"), "could not start itself"},
		"needs administrator": {errors.Join(ErrNeedsElevation, errors.New("schtasks")), "could not start itself"},
	} {
		r := newFlowRig(t)
		r.as.installErr = c.err
		if err := RunApp(context.Background(), r.env()); err == nil || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], c.want) || r.opens != 0 {
			t.Errorf("%s: err %v dialogs %q opens %d", name, err, r.dialogs, r.opens)
		}
		for _, d := range r.dialogs {
			if strings.Contains(d, "systemctl") || strings.Contains(d, "launchctl") || strings.Contains(d, "schtasks") {
				t.Errorf("%s: a command is in the dialog: %q", name, d)
			}
		}
	}
}

// A definition that is loaded and was not restarted, yet nothing answers, is restarted once.
func TestAppFlowRestartsALoadedStartThatDoesNotAnswer(t *testing.T) {
	r := newFlowRig(t)
	r.as.result = AutostartResult{WasLoaded: true}
	r.dialAt = 45 // the helper does not answer in the first wait (10 s of 250 ms polls: about 41 dials), then does
	if err := RunApp(context.Background(), r.env()); err != nil {
		t.Fatal(err)
	}
	if r.as.restarts != 1 || r.opens != 1 {
		t.Fatalf("restarts %d opens %d", r.as.restarts, r.opens)
	}
	// one that Install just restarted is not restarted again
	r = newFlowRig(t)
	r.as.result = AutostartResult{WasLoaded: true, Restarted: true}
	r.dialAt = -1
	if err := RunApp(context.Background(), r.env()); err == nil || r.as.restarts != 0 || len(r.dialogs) != 1 || !strings.Contains(r.dialogs[0], "did not start") {
		t.Fatalf("err %v restarts %d dialogs %q", err, r.as.restarts, r.dialogs)
	}
	// a restart that fails ends in the same dialog
	r = newFlowRig(t)
	r.as.result = AutostartResult{WasLoaded: true}
	r.as.restartErr = errors.New("no")
	r.dialAt = -1
	if err := RunApp(context.Background(), r.env()); err == nil || r.as.restarts != 1 || len(r.dialogs) != 1 {
		t.Fatalf("err %v restarts %d dialogs %q", err, r.as.restarts, r.dialogs)
	}
}

// The app is a Mac thing: off macOS nothing is an app launch, and the LaunchAgent cannot be written.
func TestNoAppOutsideMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this is the other systems' form")
	}
	exe := filepath.Join(t.TempDir(), "Rubi Home.app", "Contents", "MacOS", "rubi-home")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(filepath.Join(filepath.Dir(filepath.Dir(exe)), "Info.plist"),
		[]byte(`<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>com.rubi-project.home</string></dict></plist>`), 0o644)
	for _, args := range [][]string{nil, {"-psn_0_1"}} {
		if IsAppLaunch(args, exe) {
			t.Errorf("IsAppLaunch(%q) on %s", args, runtime.GOOS)
		}
	}
	if InAppBundle(exe) {
		t.Error("InAppBundle on " + runtime.GOOS)
	}
	if bundleID(filepath.Join(filepath.Dir(filepath.Dir(exe)), "Info.plist")) != "" {
		t.Error("a bundle identifier was read")
	}
	if _, err := AgentPlist("/a/b", "/a/logs"); err == nil {
		t.Error("a LaunchAgent was made on " + runtime.GOOS)
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
	if st, _ := os.Stat(hostpaths.HelperHome(home)); st.Mode().Perm() != 0o700 {
		t.Fatalf("folder mode %v", st.Mode())
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

// install-autostart: the binary is checked, the log folder made, and Install told that the binary may have changed.
func TestRunInstallAutostart(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin", "rubi-home")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("x"), 0o755)
	f := &fakeAutostart{}
	if err := RunInstallAutostart(context.Background(), f, home, bin, false); err != nil {
		t.Fatal(err)
	}
	want := AutostartSpec{Bin: bin, Logs: filepath.Join(hostpaths.HelperHome(home), "logs"), BinChanged: true}
	if len(f.installs) != 1 || f.installs[0] != want {
		t.Fatalf("%+v", f.installs)
	}
	if st, err := os.Stat(want.Logs); err != nil || !st.IsDir() {
		t.Errorf("no log folder: %v", err)
	}
	for _, bad := range []string{"", "relative/rubi-home", filepath.Join(home, "missing"), home} {
		f := &fakeAutostart{}
		if err := RunInstallAutostart(context.Background(), f, home, bad, false); err == nil || len(f.installs) != 0 {
			t.Errorf("%q: %v %v", bad, err, f.installs)
		}
	}
	f.installErr = ErrNeedsElevation
	if err := RunInstallAutostart(context.Background(), f, home, bin, false); !errors.Is(err, ErrNeedsElevation) {
		t.Errorf("%v", err)
	}
	removed := &fakeRemove{}
	if err := RunInstallAutostart(context.Background(), removed, home, "", true); err != nil || removed.n != 1 {
		t.Errorf("remove: %v %d", err, removed.n)
	}
}

type fakeRemove struct {
	fakeAutostart
	n int
}

func (f *fakeRemove) Remove(context.Context) error { f.n++; return nil }

func TestInstallAutostartOrSupervise(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin", "rubi-home")
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("x"), 0o755)
	starts := 0
	start := func() error { starts++; return nil }
	f := &fakeAutostart{}
	if sup, err := InstallAutostartOrSupervise(context.Background(), f, home, bin, start); sup || err != nil || starts != 0 {
		t.Errorf("with a service manager: %v %v, %d starts", sup, err, starts)
	}
	f.installErr = fmt.Errorf("systemd: %w", ErrNoServiceManager)
	if sup, err := InstallAutostartOrSupervise(context.Background(), f, home, bin, start); !sup || err != nil || starts != 1 {
		t.Errorf("without one: %v %v, %d starts", sup, err, starts)
	}
	f.installErr = ErrNeedsElevation
	if sup, err := InstallAutostartOrSupervise(context.Background(), f, home, bin, start); sup || !errors.Is(err, ErrNeedsElevation) || starts != 1 {
		t.Errorf("another failure: %v %v, %d starts", sup, err, starts)
	}
}
