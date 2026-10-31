package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Real output of pkgutil and spctl on container-1.6.1-installer-signed.pkg.
const pkgutilOut = `Package "c.pkg":
   Status: signed by a developer certificate issued by Apple for distribution
   Notarization: trusted by the Apple notary service
   Signed with a trusted timestamp on: 2026-10-23 23:39:42 +0000
   Certificate Chain:
    1. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)
       Expires: 2030-06-04 16:44:29 +0000
       SHA256 Fingerprint:
           45 19 B4 3D 00 F5 5E C6 31 38 D4 03 DE 1F 3C 54 23 90 EA E3 D8 2E 
           59 C1 84 38 46 FB DF E1 4F 5B
       ------------------------------------------------------------------------
    2. Developer ID Certification Authority
       Expires: 2031-09-17 00:00:00 +0000
    3. Apple Root CA
       Expires: 2035-02-09 21:40:36 +0000
`

const spctlOut = `c.pkg: accepted
source=Notarized Developer ID
origin=Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)
`

func TestCheckSignature(t *testing.T) {
	if err := checkSignature(pkgutilOut, "UPBK2H6LZM"); err != nil {
		t.Fatal(err)
	}
	if err := checkAssessment(spctlOut, "UPBK2H6LZM"); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ out, team string }{
		"wrong team":    {pkgutilOut, "AAAAAAAAAA"},
		"unsigned":      {"Package \"c.pkg\":\n   Status: no signature\n", "UPBK2H6LZM"},
		"not notarized": {strings.Replace(pkgutilOut, "Notarization: trusted by the Apple notary service", "Notarization: not notarized", 1), "UPBK2H6LZM"},
		"not trusted":   {strings.Replace(pkgutilOut, "issued by Apple for distribution", "but not trusted", 1), "UPBK2H6LZM"},
		"team in a later cert": {strings.Replace(pkgutilOut, "1. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)", "1. Developer ID Installer: Someone (BBBBBBBBBB)", 1) +
			"    4. Developer ID Installer: Apple Inc. - Containerization (UPBK2H6LZM)\n", "UPBK2H6LZM"},
		"application cert": {strings.Replace(pkgutilOut, "Developer ID Installer:", "Developer ID Application:", 1), "UPBK2H6LZM"},
	} {
		if err := checkSignature(c.out, c.team); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, out := range map[string]string{
		"rejected":      "c.pkg: rejected\nsource=Unnotarized Developer ID\n",
		"not notarized": strings.Replace(spctlOut, "Notarized Developer ID", "Developer ID", 1),
		"other origin":  strings.Replace(spctlOut, "UPBK2H6LZM", "BBBBBBBBBB", 1),
	} {
		if err := checkAssessment(out, "UPBK2H6LZM"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- download ----

type dl struct {
	t       *testing.T
	body    []byte
	rel     Release
	srv     *httptest.Server
	cdn     *httptest.Server
	handler http.HandlerFunc
}

// newDownload serves body through a redirect: srv (github.com) -> cdn (the asset host).
func newDownload(t *testing.T, body []byte, cdn http.HandlerFunc) *dl {
	t.Helper()
	d := &dl{t: t, body: body}
	if cdn == nil {
		cdn = func(w http.ResponseWriter, r *http.Request) { w.Write(body) }
	}
	d.cdn = httptest.NewTLSServer(cdn)
	d.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, d.cdn.URL+"/asset", http.StatusFound)
	}))
	t.Cleanup(d.srv.Close)
	t.Cleanup(d.cdn.Close)
	sum := sha256.Sum256(body)
	d.rel = Release{Version: "1.6.1", URL: d.srv.URL + "/pkg", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), Team: "UPBK2H6LZM"}
	return d
}

// opts points a Setup at the test servers (both use a self-signed certificate; the client trusts it).
func (d *dl) opts(o *Options) {
	d.cdn.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs.AddCert(d.srv.Certificate())
	o.HTTP = d.cdn.Client()
	o.Release = d.rel
	o.DownloadHosts = []string{strings.TrimPrefix(d.srv.URL, "https://")}
	o.RedirectHosts = []string{strings.TrimPrefix(d.cdn.URL, "https://")}
}

func TestDownload(t *testing.T) {
	body := []byte(strings.Repeat("pkg-bytes-", 1000))
	t.Run("ok through the redirect", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts)
		dir := filepath.Join(t.TempDir(), "setup")
		p, err := e.s.download(context.Background(), dir, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != string(body) {
			t.Error("content differs")
		}
		if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v", st.Mode())
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v", st.Mode())
		}
		// a second call keeps the matching file and downloads nothing
		d.cdn.Close()
		if p2, err := e.s.download(context.Background(), dir, func(string) {}); err != nil || p2 != p {
			t.Errorf("second call: %v %v", p2, err)
		}
	})
	t.Run("wrong hash deletes the file", func(t *testing.T) {
		d := newDownload(t, body, nil)
		d.rel.SHA256 = strings.Repeat("0", 64)
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		_, err := e.s.download(context.Background(), dir, func(string) {})
		if err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("err %v", err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("left behind: %v", ents)
		}
	})
	t.Run("too large", func(t *testing.T) {
		for name, h := range map[string]http.HandlerFunc{
			"declared": func(w http.ResponseWriter, r *http.Request) { w.Write(append(body, body...)) },
			"streamed": func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < 3; i++ {
					w.Write(body)
					w.(http.Flusher).Flush()
				}
			},
		} {
			d := newDownload(t, body, h)
			e := newTestEnv(t, d.opts)
			dir := t.TempDir()
			if _, err := e.s.download(context.Background(), dir, func(string) {}); err == nil || !strings.Contains(err.Error(), "larger") {
				t.Errorf("%s: err %v", name, err)
			}
			if ents, _ := os.ReadDir(dir); len(ents) != 0 {
				t.Errorf("%s: left behind: %v", name, ents)
			}
		}
	})
	t.Run("too small", func(t *testing.T) {
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) { w.Write(body[:100]) })
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		if _, err := e.s.download(context.Background(), dir, func(string) {}); err == nil || !strings.Contains(err.Error(), "size") {
			t.Errorf("err %v", err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("left behind: %v", ents)
		}
	})
	t.Run("redirect to a host that isn't allowed", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts, func(o *Options) { o.RedirectHosts = []string{"release-assets.githubusercontent.com"} })
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil || !strings.Contains(err.Error(), "isn't allowed") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("redirect to http", func(t *testing.T) {
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
		defer plain.Close()
		d := newDownload(t, body, nil)
		d.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) })
		e := newTestEnv(t, d.opts, func(o *Options) { o.RedirectHosts = append(o.RedirectHosts, strings.TrimPrefix(plain.URL, "http://")) })
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil {
			t.Error("followed a redirect to plain http")
		}
	})
	t.Run("the start must be an allowed https host", func(t *testing.T) {
		d := newDownload(t, body, nil)
		for _, u := range []string{"http://github.com/x.pkg", "https://example.com/x.pkg", "https://user@github.com/x.pkg", "ftp://github.com/x"} {
			e := newTestEnv(t, d.opts, func(o *Options) { o.Release.URL = u; o.DownloadHosts = []string{"github.com"} })
			if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil {
				t.Errorf("%s accepted", u)
			}
		}
	})
	t.Run("server error", func(t *testing.T) {
		d := newDownload(t, body, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
		e := newTestEnv(t, d.opts)
		if _, err := e.s.download(context.Background(), t.TempDir(), func(string) {}); err == nil || !strings.Contains(err.Error(), "500") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("a stale wrong file is replaced", func(t *testing.T) {
		d := newDownload(t, body, nil)
		e := newTestEnv(t, d.opts)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, d.rel.fileName()), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.download(context.Background(), dir, func(string) {}); err != nil {
			t.Fatal(err)
		}
	})
}

// ---- admin install ----

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

// ---- the container job ----

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
