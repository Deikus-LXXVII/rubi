package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// What Finder does with the Mac app: it runs Contents/MacOS/rubi-home with no arguments (very old macOS adds
// -psn_...). RunApp is then the whole install and update: copy the binary to where the script installer puts it,
// write and load the same LaunchAgent, wait for the helper, show the pairing window. Nothing is typed anywhere.

// AgentLabel is the LaunchAgent's label (and the app's bundle identifier).
const AgentLabel = "com.rubi-project.home"

const (
	appWaitLimit = 10 * time.Second
	appPoll      = 250 * time.Millisecond
	launchctl    = "/bin/launchctl"
)

// IsAppLaunch says whether Finder opened the app: no arguments (or only -psn_*), from a *.app/Contents/MacOS/ folder
// whose Contents/Info.plist names this app. A bare run of the binary elsewhere keeps printing its usage.
func IsAppLaunch(args []string, exe string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-psn_") {
			return false
		}
	}
	return InAppBundle(exe) && bundleID(filepath.Join(filepath.Dir(filepath.Dir(exe)), "Info.plist")) == AgentLabel
}

// bundleID is the CFBundleIdentifier of an XML Info.plist (the build script writes one), or "".
func bundleID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	dec := xml.NewDecoder(io.LimitReader(f, 64<<10))
	var key, text string
	var in, wantValue bool
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch e := tok.(type) {
		case xml.StartElement:
			if e.Name.Local == "key" || e.Name.Local == "string" {
				in, text = true, ""
			}
		case xml.CharData:
			if in {
				text += string(e)
			}
		case xml.EndElement:
			switch e.Name.Local {
			case "key":
				key, in, wantValue = text, false, true
			case "string":
				in = false
				if wantValue && key == "CFBundleIdentifier" {
					return strings.TrimSpace(text)
				}
				wantValue = false
			}
		}
	}
}

// UserHome is the user's home folder from the account database (the environment can lie), else $HOME; it must be
// an absolute path.
func UserHome() (string, error) {
	h := ""
	if u, err := user.Current(); err == nil {
		h = u.HomeDir
	}
	if h == "" {
		h, _ = os.UserHomeDir()
	}
	if !filepath.IsAbs(h) {
		return "", errors.New("the home folder is not an absolute path")
	}
	return filepath.Clean(h), nil
}

// InAppBundle is true for <name>.app/Contents/MacOS/<file>.
func InAppBundle(exe string) bool {
	if exe == "" {
		return false
	}
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	return filepath.Base(macos) == "MacOS" && filepath.Base(contents) == "Contents" &&
		strings.HasSuffix(filepath.Base(filepath.Dir(contents)), ".app")
}

// AgentPlistTemplate is the LaunchAgent that panel/install-home.sh writes, with its two variables: Bin is the
// installed binary and Logs the log folder. A test keeps the script and this one the same.
const AgentPlistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.rubi-project.home</string>
  <key>ProgramArguments</key><array><string>{{xml .Bin}}</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>{{xml .Logs}}/rubi-home.log</string>
  <key>StandardErrorPath</key><string>{{xml .Logs}}/rubi-home.log</string>
</dict>
</plist>
`

var agentPlist = template.Must(template.New("agent").Funcs(template.FuncMap{"xml": func(s string) (string, error) {
	var b bytes.Buffer
	err := xml.EscapeText(&b, []byte(s))
	return b.String(), err
}}).Parse(AgentPlistTemplate))

// AgentPlist is the LaunchAgent file for a binary and a log folder.
func AgentPlist(bin, logs string) (string, error) {
	var b bytes.Buffer
	err := agentPlist.Execute(&b, struct{ Bin, Logs string }{bin, logs})
	return b.String(), err
}

// AppEnv is what RunApp touches; tests replace every part.
type AppEnv struct {
	Home      string // the user's home folder (~/.rubi-home and ~/Library/LaunchAgents are in it)
	UID       int
	Exe       string // the running binary, inside the app
	HelperDir string // the helper's own folder (DefaultDir): its socket is there
	Self      string // this app's version (version.Version when empty)
	HasLog    bool   // the app log is being written, so a dialog may point to it
	Run       RunFunc
	Version   func(path string) (string, error) // `path version` (an installed binary's version)
	Now       func() time.Time
	Sleep     func(time.Duration)
	// Dial connects to the helper's socket: nil error when something listens. It also returns the program file of
	// the listener when the OS can say ("" when not).
	Dial func(ctx context.Context, socket string) (string, error)
	Open func(ctx context.Context) error // the pairing window (RunOpen on the opened helper)
	Logf func(format string, args ...any)

	CallTimeout   time.Duration // one launchctl call (20 s)
	DialogTimeout time.Duration // an error or notice dialog (2 minutes)
	OpenTimeout   time.Duration // the pairing window, at the user's pace but not forever (10 minutes)
}

// appError is a failure with the sentence the user sees (never a command or a path of the system) and the
// detail for the log.
type appError struct {
	user string
	err  error
}

func (e *appError) Error() string { return e.user + ": " + e.err.Error() }
func (e *appError) Unwrap() error { return e.err }

func appFail(user string, err error) error { return &appError{user, err} }

const appLogHint = " The details are in the file rubi-home-app.log in the folder .rubi-home/logs in your home folder."

func (env *AppEnv) defaults() {
	if env.Run == nil {
		env.Run = execRun
	}
	if env.Version == nil {
		env.Version = func(p string) (string, error) { return runVersion(p, HookBinaryTimeout) }
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.Sleep == nil {
		env.Sleep = time.Sleep
	}
	if env.Dial == nil {
		env.Dial = dialSocket
	}
	if env.Logf == nil {
		env.Logf = func(string, ...any) {}
	}
	if env.Self == "" {
		env.Self = version.Version
	}
	if env.CallTimeout == 0 {
		env.CallTimeout = 20 * time.Second
	}
	if env.DialogTimeout == 0 {
		env.DialogTimeout = 2 * time.Minute
	}
	if env.OpenTimeout == 0 {
		env.OpenTimeout = 10 * time.Minute
	}
}

// show puts up a one-button dialog; it gives up after DialogTimeout.
func (env AppEnv) show(ctx context.Context, text string) {
	ctx, cancel := context.WithTimeout(ctx, env.DialogTimeout)
	defer cancel()
	if _, err := dialog(ctx, OpenEnv{Run: env.Run}, text, btnOK); err != nil {
		env.Logf("rubi-home app: a dialog: %v", err)
	}
}

// launch runs launchctl under CallTimeout.
func (env AppEnv) launch(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, env.CallTimeout)
	defer cancel()
	_, err := env.Run(ctx, "", launchctl, args...)
	return err
}

// RunApp is what opening the app does. A failure is logged and shown in a dialog (the dialog has no command in
// it); the error is also returned, for the exit status.
func RunApp(ctx context.Context, env AppEnv) error {
	env.defaults()
	err := runApp(ctx, env)
	if err == nil {
		return nil
	}
	env.Logf("rubi-home app: %v", err)
	text := "Rubi Home could not finish setting up on this Mac."
	var ae *appError
	if errors.As(err, &ae) {
		text = ae.user
	}
	if env.HasLog {
		text += appLogHint
	}
	env.show(ctx, text)
	return err
}

func runApp(ctx context.Context, env AppEnv) error {
	if !filepath.IsAbs(env.Home) || env.Exe == "" || env.HelperDir == "" {
		return appFail("Rubi Home can't find your home folder or its own data folder.", errors.New("home, executable or data folder unknown"))
	}
	dir := hostpaths.HelperHome(env.Home)
	bin := filepath.Join(dir, "bin", "rubi-home")
	logs := filepath.Join(dir, "logs")

	if err := prepareDirs(dir, env.UID); err != nil {
		return appFail("Rubi Home's folder isn't safe to install into.", err)
	}
	res, err := installBinary(env, dir)
	if err != nil {
		return appFail("Rubi Home could not copy itself into your home folder.", err)
	}
	if res.keptNewer {
		env.show(ctx, fmt.Sprintf("A newer Rubi Home (%s) is already installed; this app is %s. Keep the newer one.", shownVersion(res.installed), shownVersion(env.Self)))
	} else if res.kept {
		env.show(ctx, fmt.Sprintf("Another build of Rubi Home (%s) is already installed; this app is %s. The installed one stays.", shownVersion(res.installed), shownVersion(env.Self)))
	}
	plist := filepath.Join(env.Home, "Library", "LaunchAgents", AgentLabel+".plist")
	plistChanged, err := writeAgent(plist, bin, logs, env.Logf)
	if err != nil {
		return appFail("Rubi Home could not set itself up to start when you log in.", err)
	}
	loaded, restarted, err := loadAgent(ctx, env, plist, res.changed, plistChanged)
	if err != nil {
		return appFail("Rubi Home could not start itself.", err)
	}
	err = waitForHelper(ctx, env, bin)
	if err != nil && loaded && !restarted && ctx.Err() == nil {
		// Loaded, yet nothing answers: the process is stuck or gone. Restart it once.
		env.Logf("rubi-home app: the loaded agent does not answer; restarting it: %v", err)
		if kerr := env.launch(ctx, "kickstart", "-k", fmt.Sprintf("gui/%d/%s", env.UID, AgentLabel)); kerr != nil {
			return appFail("Rubi Home was installed but did not start. Open it again in a minute.", fmt.Errorf("restarting the agent: %w", kerr))
		}
		err = waitForHelper(ctx, env, bin)
	}
	if err != nil {
		return appFail("Rubi Home was installed but did not start. Open it again in a minute.", err)
	}
	octx, cancel := context.WithTimeout(ctx, env.OpenTimeout)
	defer cancel()
	if err := env.Open(octx); err != nil {
		if octx.Err() != nil && ctx.Err() == nil { // the window was left open too long: nothing failed
			env.Logf("rubi-home app: the window was closed after %v", env.OpenTimeout)
			return nil
		}
		return appFail("Rubi Home could not show its window.", err)
	}
	return nil
}

// prepareDirs makes ~/.rubi-home (0700), bin and logs (0755) and checks each is a real folder (not a symlink) that
// the user owns and nobody else can write to.
func prepareDirs(dir string, uid int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, d := range []string{"bin", "logs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {filepath.Join(dir, "bin"), 0o755}, {filepath.Join(dir, "logs"), 0o755}} {
		fi, err := os.Lstat(c.path)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a real folder", c.path)
		}
		if owner, _, ok := fsx.OwnerOf(fi); !ok || int(owner) != uid {
			return fmt.Errorf("%s is not owned by this user", c.path)
		}
		if err := os.Chmod(c.path, c.mode); err != nil {
			return err
		}
		if fi, err = os.Lstat(c.path); err != nil || fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s can be written by others", c.path)
		}
	}
	return nil
}

type installResult struct {
	changed   bool   // the installed binary was created or replaced
	kept      bool   // a different installed binary was kept
	keptNewer bool   // ... because it is newer
	installed string // its version
}

// keepInstalled decides whether an installed binary, whose version was read (or not), stays. A newer release stays;
// so does any build when only one of the two is a development build. Both development builds, an unreadable
// version and an older or equal release are replaced.
func keepInstalled(installed string, readErr error, self string) (keep, newer bool) {
	if readErr != nil {
		return false, false
	}
	_, _, iok := semver(installed)
	_, _, sok := semver(self)
	switch {
	case iok && sok:
		return versionOlder(self, installed), true
	case !iok && !sok:
		return false, false
	}
	return true, false
}

// installBinary puts a copy of the running binary at dir/bin/rubi-home: the same place and mode as the script
// installer. An equal copy is left alone, and so is a newer one (an old download must not downgrade Rubi Home).
// A symlink in the place is never "equal": it is replaced by a file.
func installBinary(env AppEnv, dir string) (installResult, error) {
	var res installResult
	dst := filepath.Join(dir, "bin", "rubi-home")
	srcSum, err := fileSum(env.Exe)
	if err != nil {
		return res, err
	}
	if fi, err := os.Lstat(dst); err == nil && fi.Mode().IsRegular() {
		if dstSum, err := fileSum(dst); err == nil && dstSum == srcSum {
			return res, nil
		}
		v, verr := env.Version(dst) // only ever a regular file: a symlink could point at anything
		if verr != nil {
			env.Logf("rubi-home app: the installed version can't be read, replacing it: %v", verr)
		}
		if keep, newer := keepInstalled(v, verr, env.Self); keep {
			res.kept, res.keptNewer, res.installed = true, newer, v
			return res, nil
		}
	} else if err == nil {
		env.Logf("rubi-home app: %s is not a regular file; replacing it", dst)
	}
	src, err := os.Open(env.Exe)
	if err != nil {
		return res, err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".rubi-home-new-*")
	if err != nil {
		return res, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return res, err
	}
	if err := tmp.Sync(); err != nil {
		return res, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return res, err
	}
	ok = true
	res.changed = true
	return res, nil
}

// fileSum is the SHA-256 of a regular file; a symlink (Lstat) or anything else is an error.
func fileSum(path string) ([32]byte, error) {
	var sum [32]byte
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return sum, errors.New("not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// writeAgent writes the LaunchAgent file unless it is already exactly this; it reports whether it wrote. A symlink
// in its place is replaced by a regular file (the rename does that) and logged.
func writeAgent(path, bin, logs string, logf func(string, ...any)) (bool, error) {
	want, err := AgentPlist(bin, logs)
	if err != nil {
		return false, err
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().IsRegular() {
			if have, err := os.ReadFile(path); err == nil && string(have) == want {
				return false, nil
			}
		} else {
			logf("rubi-home app: %s is not a regular file; replacing it", path)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rubi-home-agent-*")
	if err != nil {
		return false, err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(want); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	ok = true
	return true, nil
}

// loadAgent starts the agent: a new one (or a changed file) is booted out, if there, and bootstrapped; an
// agent that is loaded with the same file is restarted only when the binary changed. It reports whether the
// agent was loaded before and whether this call started (or restarted) it.
func loadAgent(ctx context.Context, env AppEnv, plist string, binChanged, plistChanged bool) (loaded, restarted bool, err error) {
	domain := fmt.Sprintf("gui/%d", env.UID)
	loaded = env.launch(ctx, "print", domain+"/"+AgentLabel) == nil
	if !loaded || plistChanged {
		if err := env.launch(ctx, "bootout", domain, plist); err != nil {
			env.Logf("rubi-home app: nothing to unload: %v", err) // normal for a first install
		}
		if err := env.launch(ctx, "bootstrap", domain, plist); err != nil {
			// The unload can still be settling, or the user switched the agent off in Login Items: enable it, then once more.
			env.Logf("rubi-home app: loading the agent failed, enabling it and trying again: %v", err)
			if eerr := env.launch(ctx, "enable", domain+"/"+AgentLabel); eerr != nil {
				env.Logf("rubi-home app: enabling the agent: %v", eerr)
			}
			env.Sleep(time.Second)
			if err := env.launch(ctx, "bootstrap", domain, plist); err != nil {
				return loaded, false, fmt.Errorf("loading the agent: %w", err)
			}
		}
		return loaded, true, nil
	}
	if binChanged {
		if err := env.launch(ctx, "kickstart", "-k", domain+"/"+AgentLabel); err != nil {
			return loaded, false, fmt.Errorf("restarting the agent: %w", err)
		}
		return loaded, true, nil
	}
	return loaded, false, nil
}

// waitForHelper waits up to ten seconds for the helper to listen on its local socket: the first thing a
// running helper offers (Claude Code's hooks use it), so nothing is guessed from a file that may be left over.
// When the OS says which program answers and it is not the installed binary, that is logged, not refused.
func waitForHelper(ctx context.Context, env AppEnv, bin string) error {
	socket := ClaudeSocketPath(env.HelperDir)
	deadline := env.Now().Add(appWaitLimit)
	var last error
	for {
		exe, derr := env.Dial(ctx, socket)
		if derr == nil {
			switch {
			case exe == "":
				env.Logf("rubi-home app: the program that answers on the helper's socket could not be identified")
			case !sameFile(exe, bin):
				env.Logf("rubi-home app: the helper that answers is another program than the installed one")
			}
			return nil
		}
		last = derr
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !env.Now().Before(deadline) {
			return fmt.Errorf("the helper did not answer within %v: %w", appWaitLimit, last)
		}
		env.Sleep(appPoll)
	}
}

func sameFile(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}

func dialSocket(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return "", err
	}
	defer c.Close()
	exe, _ := peerExe(c) // best effort: the caller says "unknown"
	return exe, nil
}

// OpenAppLog opens the app's log, ~/.rubi-home/logs/rubi-home-app.log (private); the caller closes it. Failing
// that it returns nil.
func OpenAppLog(homeDir string) *os.File {
	dir := hostpaths.HelperHome(homeDir)
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(logs, "rubi-home-app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
