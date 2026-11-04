//go:build darwin

package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The administrator install of Apple's container tool: a Mac only (osascript, pkgutil, stat -f). Split from container_test.go.

func TestInstallCommandQuoting(t *testing.T) {
	sha := Pinned.SHA256
	for _, pkg := range []string{
		"/Users/alice/Library/Application Support/Rubi Home/setup/c.pkg",
		"/tmp/it's/a'; touch /tmp/pwned; echo '.pkg",
		`/tmp/with"quote/$(touch /tmp/pwned)/` + "`id`.pkg",
		`/tmp/back\slash/c.pkg`,
		"/tmp/'/c.pkg",
	} {
		argv, err := installCommand(pkg, sha, "UPBK2H6LZM")
		if err != nil {
			t.Fatal(err)
		}
		if len(argv) != 3 || argv[0] != "/usr/bin/osascript" || argv[1] != "-e" {
			t.Fatalf("argv %v", argv)
		}
		script := argv[2]
		if !strings.HasPrefix(script, `do shell script "`) || !strings.HasSuffix(script, `" with administrator privileges`) {
			t.Fatalf("script %q", script)
		}
		// undo the AppleScript string escaping, then split the shell words as sh would
		lit := strings.TrimSuffix(strings.TrimPrefix(script, `do shell script `), ` with administrator privileges`)
		sh := unAS(t, lit)
		if !strings.HasPrefix(sh, "/bin/sh -c ") {
			t.Fatalf("sh %q", sh)
		}
		words := shellWords(t, sh)
		if len(words) != 8 || words[3] != "rubi-install" || words[4] != pkg || words[5] != sha || words[6] != "UPBK2H6LZM" || words[7] != "0" || words[2] != installScript {
			t.Errorf("words: %q (pkg %q)", words, pkg)
		}
	}
	for _, bad := range []string{"", "relative/c.pkg", "/tmp/a\nb.pkg", "/tmp/a\x00b.pkg", "/tmp/a\rb"} {
		if _, err := installCommand(bad, Pinned.SHA256, "UPBK2H6LZM"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := installCommand("/tmp/c.pkg", "abc\ndef", "UPBK2H6LZM"); err == nil {
		t.Error("hash with newline accepted")
	}
}

// unAS reads an AppleScript string literal made by asString.
func unAS(t *testing.T, lit string) string {
	t.Helper()
	if len(lit) < 2 || lit[0] != '"' || lit[len(lit)-1] != '"' {
		t.Fatalf("not a literal: %q", lit)
	}
	in := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] == '\\' {
			i++
			if i >= len(in) || in[i] != '\\' && in[i] != '"' {
				t.Fatalf("bad escape in %q", lit)
			}
		} else if in[i] == '"' {
			t.Fatalf("unescaped quote in %q", lit)
		}
		b.WriteByte(in[i])
	}
	return b.String()
}

// shellWords splits a command line made only of single-quoted words and plain words, the way sh would.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	i := 0
	for i < len(s) {
		if s[i] == ' ' {
			i++
			continue
		}
		var w strings.Builder
		for i < len(s) && s[i] != ' ' {
			switch s[i] {
			case '\'':
				j := strings.IndexByte(s[i+1:], '\'')
				if j < 0 {
					t.Fatalf("unterminated quote in %q", s)
				}
				w.WriteString(s[i+1 : i+1+j])
				i += j + 2
			case '\\':
				if i+1 >= len(s) {
					t.Fatal("trailing backslash")
				}
				w.WriteByte(s[i+1]) // only produced as \' between quoted parts
				i += 2
			default:
				w.WriteByte(s[i])
				i++
			}
		}
		words = append(words, w.String())
	}
	return words
}

// The root script's early exits are real shell: a missing file, a symlink and a wrong hash never get to
// the installer. (The script is run with sh only; osascript and installer are never run.)
func TestInstallScriptRefusals(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the script uses macOS tools (stat -f, shasum, /private/var/tmp)")
	}
	uid := strconv.Itoa(os.Getuid())
	dir := t.TempDir()
	good := filepath.Join(dir, "c.pkg")
	if err := os.WriteFile(good, []byte("not a package"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.pkg")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("not a package"))
	for name, c := range map[string]struct {
		pkg, sha string
		code     int
	}{
		"missing":    {filepath.Join(dir, "none.pkg"), Pinned.SHA256, 10},
		"symlink":    {link, hex.EncodeToString(sum[:]), 10},
		"wrong hash": {good, Pinned.SHA256, 11},
		// the right hash of a file that isn't a signed package stops at the signature check
		"unsigned": {good, hex.EncodeToString(sum[:]), 12},
	} {
		cmd := exec.Command("/bin/sh", "-c", installScript, "rubi-install", c.pkg, c.sha, "UPBK2H6LZM", uid)
		cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + dir}
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != c.code {
			t.Errorf("%s: %v (%s), want exit %d", name, err, out, c.code)
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("the script left files behind: %v", ents)
	}
	// not running as the uid it expects (a non-root user asked for root): refused before anything is copied
	cmd := exec.Command("/bin/sh", "-c", installScript, "rubi-install", good, hex.EncodeToString(sum[:]), "UPBK2H6LZM", "4242")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 13 {
		t.Errorf("wrong uid: %v", err)
	}
	if !strings.Contains(installScript, "mktemp -d /private/var/tmp/rubi-container.XXXXXX") || !strings.Contains(installScript, "unset TMPDIR") {
		t.Error("the scratch directory isn't the fixed private one")
	}
	// the copy never follows a link, is re-checked as a plain file, and only then hashed
	iCp := strings.Index(installScript, `cp -P "$src" "$pkg"`)
	iChk := strings.Index(installScript, `[ -L "$pkg" ] || [ ! -f "$pkg" ]`)
	iSum := strings.Index(installScript, `shasum -a 256 "$pkg"`)
	if iCp < 0 || iChk < iCp || iSum < iChk {
		t.Errorf("copy/check/hash order wrong: %d %d %d", iCp, iChk, iSum)
	}
}

func TestCancelledMapping(t *testing.T) {
	for out, want := range map[string]bool{
		"execution error: User canceled. (-128)":        true,
		"0:94: execution error: User cancelled. (-128)": true,
		"execution error: the installer failed (1)":     false,
		"": false,
	} {
		if cancelled(out) != want {
			t.Errorf("%q", out)
		}
	}
}

// containerEnv is a Mac with no container tool, a download server, and a scripted pkgutil, spctl and osascript.
type containerEnv struct {
	*testEnv
	d         *dl
	installed bool
	osa       func(argv []string) ([]byte, error)
	osaCalls  [][]string
}

func newContainerEnv(t *testing.T) *containerEnv {
	body := []byte(strings.Repeat("pkg-bytes-", 100))
	ce := &containerEnv{d: newDownload(t, body, nil)}
	ce.testEnv = newTestEnv(t, ce.d.opts)
	ce.osa = func(argv []string) ([]byte, error) { ce.installed = true; return nil, nil }
	ce.m.extra = func(argv []string) ([]byte, error, bool) {
		switch argv[0] {
		case "/usr/sbin/pkgutil":
			return []byte(pkgutilOut), nil, true
		case "/usr/sbin/spctl":
			return []byte(spctlOut), nil, true
		case "/usr/bin/osascript":
			ce.osaCalls = append(ce.osaCalls, argv)
			out, err := ce.osa(argv)
			return out, err, true
		}
		if strings.Join(argv, " ") == "container --version" {
			if !ce.installed {
				return nil, errors.New("not found"), true
			}
			return []byte(version161), nil, true
		}
		return nil, nil, false
	}
	return ce
}

func (c *containerEnv) runJob() *Setup {
	c.t.Helper()
	if err := c.s.Run(context.Background(), "container", "", "", ""); err != nil {
		c.t.Fatal(err)
	}
	c.wait()
	return c.s
}

func TestContainerInstall(t *testing.T) {
	c := newContainerEnv(t)
	c.runJob()
	if len(c.osaCalls) != 1 || !strings.Contains(c.osaCalls[0][2], "with administrator privileges") {
		t.Fatalf("osascript calls: %v", c.osaCalls)
	}
	if !strings.Contains(c.osaCalls[0][2], filepath.Join(c.dir, "setup", c.d.rel.fileName())) {
		t.Errorf("the script doesn't name the package inside Rubi Home's directory: %s", c.osaCalls[0][2])
	}
	if st := c.step("container"); st.State != Done {
		t.Errorf("after install: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "setup", c.d.rel.fileName())); err == nil {
		t.Error("the package was kept after a successful install")
	}
	// pressing it again installs nothing
	c.runJob()
	if len(c.osaCalls) != 1 {
		t.Error("reinstalled")
	}
}

func TestContainerInstallCancelledDialog(t *testing.T) {
	c := newContainerEnv(t)
	c.osa = func(argv []string) ([]byte, error) {
		return []byte("42:57: execution error: User canceled. (-128)"), errors.New("exit status 1")
	}
	c.runJob()
	st := c.step("container")
	if st.State != NeedsUser || st.Reason != "The administrator dialog was cancelled." {
		t.Errorf("%+v", st)
	}
	// the verified download is kept, so the next try goes straight to the dialog
	if _, err := os.Stat(filepath.Join(c.dir, "setup", c.d.rel.fileName())); err != nil {
		t.Error("the package was deleted")
	}
	c.d.cdn.Close()
	c.osa = func(argv []string) ([]byte, error) { c.installed = true; return nil, nil }
	c.runJob()
	if st := c.step("container"); st.State != Done {
		t.Errorf("second try: %+v", st)
	}
}

func TestContainerInstallRefusals(t *testing.T) {
	t.Run("bad signature", func(t *testing.T) {
		c := newContainerEnv(t)
		prev := c.m.extra
		c.m.extra = func(argv []string) ([]byte, error, bool) {
			if argv[0] == "/usr/sbin/pkgutil" {
				return []byte(strings.ReplaceAll(pkgutilOut, "UPBK2H6LZM", "BBBBBBBBBB")), nil, true
			}
			return prev(argv)
		}
		c.runJob()
		if st := c.step("container"); st.State != Failed || len(c.osaCalls) != 0 {
			t.Errorf("%+v, osascript %v", st, c.osaCalls)
		}
		if _, err := os.Stat(filepath.Join(c.dir, "setup", c.d.rel.fileName())); err == nil {
			t.Error("the unverified package was kept")
		}
	})
	t.Run("not accepted by spctl", func(t *testing.T) {
		c := newContainerEnv(t)
		prev := c.m.extra
		c.m.extra = func(argv []string) ([]byte, error, bool) {
			if argv[0] == "/usr/sbin/spctl" {
				return []byte("c.pkg: rejected\n"), errors.New("exit status 3"), true
			}
			return prev(argv)
		}
		c.runJob()
		if st := c.step("container"); st.State != Failed || len(c.osaCalls) != 0 {
			t.Errorf("%+v", st)
		}
	})
	t.Run("wrong hash", func(t *testing.T) {
		c := newContainerEnv(t)
		c.s.opt.Release.SHA256 = strings.Repeat("1", 64)
		c.runJob()
		if st := c.step("container"); st.State != Failed || len(c.osaCalls) != 0 {
			t.Errorf("%+v", st)
		}
	})
	t.Run("installer fails", func(t *testing.T) {
		c := newContainerEnv(t)
		c.osa = func(argv []string) ([]byte, error) {
			return []byte("installer: failed with token=abc"), errors.New("exit 1")
		}
		c.runJob()
		st := c.step("container")
		if st.State != Failed || strings.Contains(st.Reason, "token") {
			t.Errorf("%+v", st)
		}
	})
	t.Run("not a Mac", func(t *testing.T) {
		c := newContainerEnv(t)
		c.s.opt.GOOS = "linux"
		c.runJob()
		if st := c.step("container"); st.State != Failed || len(c.osaCalls) != 0 || c.cmd.ran("/usr/sbin/pkgutil") != 0 {
			t.Errorf("%+v", st)
		}
	})
}

func TestContainerUpgradeAndNeverDowngrade(t *testing.T) {
	c := newContainerEnv(t)
	prev := c.m.extra
	ver := "container CLI version 1.5.0 (build: release, commit: abc)\n"
	c.m.extra = func(argv []string) ([]byte, error, bool) {
		if strings.Join(argv, " ") == "container --version" {
			if c.installed {
				return []byte(version161), nil, true
			}
			return []byte(ver), nil, true
		}
		return prev(argv)
	}
	if st := c.step("container"); st.Action != "upgrade" {
		t.Fatalf("%+v", st)
	}
	c.runJob()
	if len(c.osaCalls) != 1 || c.step("container").State != Done {
		t.Errorf("upgrade: %v", c.osaCalls)
	}
	// a newer tool than the pin is left alone
	c2 := newContainerEnv(t)
	prev2 := c2.m.extra
	c2.m.extra = func(argv []string) ([]byte, error, bool) {
		if strings.Join(argv, " ") == "container --version" {
			return []byte("container CLI version 2.0.0 (build: release, commit: abc)\n"), nil, true
		}
		return prev2(argv)
	}
	c2.runJob()
	if len(c2.osaCalls) != 0 || c2.cmd.ran("/usr/sbin/pkgutil") != 0 {
		t.Error("touched a newer installation")
	}
}

func TestContainerUnreadableVersionIsNeverInstalledOver(t *testing.T) {
	for _, step := range []string{"container", "all"} {
		c := newContainerEnv(t)
		prev := c.m.extra
		c.m.extra = func(argv []string) ([]byte, error, bool) {
			if strings.Join(argv, " ") == "container --version" {
				return []byte("container: something unexpected\n"), nil, true
			}
			return prev(argv)
		}
		if err := c.s.Run(context.Background(), step, "", "", ""); err != nil {
			t.Fatal(err)
		}
		c.wait()
		st := c.step("container")
		want, state := "not reinstalling", Failed
		if step == "all" { // "all" never installs: it stops and asks
			want, state = "asks for approval", NeedsUser
		}
		if st.State != state || !strings.Contains(st.Reason, want) || len(c.osaCalls) != 0 || c.cmd.ran("/usr/sbin/pkgutil") != 0 {
			t.Errorf("%s: %+v osascript %v", step, st, c.osaCalls)
		}
	}
}

func TestCancelDuringInstallWaitsForTheScript(t *testing.T) {
	c := newContainerEnv(t)
	release := make(chan struct{})
	started := make(chan struct{})
	c.osa = func(argv []string) ([]byte, error) {
		close(started)
		<-release
		c.installed = true
		return nil, nil
	}
	if err := c.s.Run(context.Background(), "container", "", "", ""); err != nil {
		t.Fatal(err)
	}
	<-started
	c.s.Cancel()
	time.Sleep(100 * time.Millisecond)
	if !c.s.Busy() {
		t.Fatal("the job was released while the privileged script may still run")
	}
	if _, err := c.op("run", map[string]any{"step": "container"}); err == nil {
		t.Error("a second install could start")
	}
	st := c.s.Status(context.Background())
	if st.Steps[0].State != Working || !strings.Contains(strings.Join(st.Steps[0].Progress, " "), "waiting for the installer") {
		t.Errorf("status while waiting: %+v", st.Steps[0])
	}
	close(release)
	c.wait()
	if st := c.step("container"); st.State != Done {
		t.Errorf("after the script ended: %+v", st)
	}
}

func TestInstallTimeoutSaysSo(t *testing.T) {
	old := installLimit
	installLimit = 100 * time.Millisecond
	defer func() { installLimit = old }()
	c := newContainerEnv(t)
	c.m.extra = func(argv []string) ([]byte, error, bool) {
		if argv[0] == "/usr/bin/osascript" {
			time.Sleep(300 * time.Millisecond)
			return nil, errors.New("killed"), true
		}
		return nil, nil, false
	}
	prev := c.m.extra
	c.m.extra = func(argv []string) ([]byte, error, bool) {
		switch argv[0] {
		case "/usr/sbin/pkgutil":
			return []byte(pkgutilOut), nil, true
		case "/usr/sbin/spctl":
			return []byte(spctlOut), nil, true
		}
		return prev(argv)
	}
	c.runJob()
	if st := c.step("container"); st.State != Failed || !strings.Contains(st.Reason, "half done") {
		t.Errorf("%+v", st)
	}
}
