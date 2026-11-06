package home

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/t3bump"
)

type tarEntry struct {
	name, body, link string
	typ              byte
	mode             int64
}

// archive builds a .tar.gz.
func archive(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: mode, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	testVer  = "9.9.9-test"
	testNode = "v99.0.0"
	testPlat = "test-plat"
)

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func sri512(b []byte) string {
	s := sha512.Sum512(b)
	return "sha512-" + base64.StdEncoding.EncodeToString(s[:])
}

// installKit is three small archives that look like Node, the T3 package and the T3 release, a pin that names them,
// and a fetch that serves them by address.
type installKit struct {
	node, pkg, rt []byte
	plat          string
	pin           t3Pin
	mu            sync.Mutex
	asked         []string
}

func newKit(t *testing.T) *installKit {
	t.Helper()
	k := &installKit{plat: testPlat}
	k.node = archive(t,
		tarEntry{name: "node-" + testNode + "-" + testPlat + "/", typ: tar.TypeDir},
		tarEntry{name: "node-" + testNode + "-" + testPlat + "/bin/node", body: "#!/bin/sh\necho node\n", mode: 0o755},
		tarEntry{name: "node-" + testNode + "-" + testPlat + "/bin/npm", typ: tar.TypeSymlink, link: "../lib/npm.js"},
		tarEntry{name: "node-" + testNode + "-" + testPlat + "/lib/npm.js", body: "//npm\n"})
	k.pkg = archive(t,
		tarEntry{name: "package/package.json", body: `{"name":"t3","version":"` + testVer + `"}`},
		tarEntry{name: "package/bin/t3.mjs", body: "// launcher\n", mode: 0o755})
	top := "t3-" + testVer + "-" + testPlat
	k.rt = archive(t,
		tarEntry{name: top + "/", typ: tar.TypeDir},
		tarEntry{name: top + "/t3", body: "#!/bin/sh\necho t3\n", mode: 0o755},
		tarEntry{name: top + "/client/index.html", body: "<html>"})
	k.setPin()
	return k
}

func (k *installKit) setPin() {
	k.pin = t3Pin{Version: testVer, Integrity: sri512(k.pkg)}
	k.pin.Runtime.SHA256 = map[string]string{k.plat: sha256hex(k.rt)}
	k.pin.Node.Version = testNode
	k.pin.Node.SHA256 = map[string]string{k.plat: sha256hex(k.node)}
}

func (k *installKit) fetch(ctx context.Context, url string, w io.Writer, max int64) error {
	k.mu.Lock()
	k.asked = append(k.asked, url)
	k.mu.Unlock()
	var b []byte
	switch {
	case strings.HasPrefix(url, "https://nodejs.org/dist/"):
		b = k.node
	case strings.HasPrefix(url, "https://registry.npmjs.org/t3/-/"):
		b = k.pkg
	case strings.HasPrefix(url, "https://github.com/pingdotgg/t3code/releases/download/"):
		b = k.rt
	default:
		return errors.New("unexpected address " + url)
	}
	_, err := w.Write(b)
	return err
}

func installHome(t *testing.T, k *installKit) *T3Home {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.T3.Fetch, h.T3.Pin, h.T3.Platform = k.fetch, &k.pin, func() string { return k.plat }
	return h.T3
}

func TestInstallDownloadsChecksAndUnpacksThePin(t *testing.T) {
	k := newKit(t)
	th := installHome(t, k)
	if _, ok := th.installedNow(); ok {
		t.Fatal("installed before the install")
	}
	if err := th.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The three addresses are the fixed ones for the pin: official Node, the npm package, the T3 release.
	want := []string{
		"https://nodejs.org/dist/" + testNode + "/node-" + testNode + "-" + testPlat + ".tar.gz",
		"https://registry.npmjs.org/t3/-/t3-" + testVer + ".tgz",
		"https://github.com/pingdotgg/t3code/releases/download/v" + testVer + "/t3-" + testVer + "-" + testPlat + ".tar.gz",
	}
	if strings.Join(k.asked, "\n") != strings.Join(want, "\n") {
		t.Fatalf("downloads:\n%s\nwant:\n%s", strings.Join(k.asked, "\n"), strings.Join(want, "\n"))
	}
	if _, ok := th.installedNow(); !ok {
		t.Fatal("not installed after the install")
	}
	for _, f := range []string{th.nodeBin(), th.pkgBin(), th.exePath(testVer)} {
		st, err := os.Stat(f)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s: %v", f, err)
		}
	}
	if st, _ := os.Stat(th.nodeBin()); st.Mode().Perm()&0o100 == 0 {
		t.Error("node lost its executable bit")
	}
	if st, _ := os.Stat(th.exePath(testVer)); st.Mode().Perm()&0o100 == 0 {
		t.Error("t3 lost its executable bit")
	}
	if l, err := os.Readlink(filepath.Join(th.runtimeDir(), "node", "bin", "npm")); err != nil || l != "../lib/npm.js" {
		t.Errorf("relative link inside the archive: %q %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(th.runtimeHome(), "runtime", "versions", testVer, ".install-complete")); strings.TrimSpace(string(b)) != testVer {
		t.Errorf("completion marker: %q", b)
	}
	// No staging or download folder is left behind.
	ents, _ := os.ReadDir(th.root())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "runtime.") || strings.HasPrefix(e.Name(), "download-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	// A changed pin (a new T3 version) is not "installed" until it is installed.
	k.pin.Version = "9.9.10-test"
	if _, ok := th.installedNow(); ok {
		t.Error("an install of another version counts as installed")
	}
	k.pin.Version = testVer
	k.pin.Node.Version = "v99.0.1"
	if _, ok := th.installedNow(); ok {
		t.Error("an install with another Node counts as installed")
	}
}

// A download that does not match its pinned hash is never unpacked, and nothing from it is put in place.
func TestInstallRefusesWhatDoesNotMatchThePin(t *testing.T) {
	for name, tamper := range map[string]func(k *installKit){
		"node":    func(k *installKit) { k.pin.Node.SHA256[testPlat] = sha256hex([]byte("other")) },
		"package": func(k *installKit) { k.pin.Integrity = sri512([]byte("other")) },
		"runtime": func(k *installKit) { k.pin.Runtime.SHA256[testPlat] = sha256hex([]byte("other")) },
	} {
		t.Run(name, func(t *testing.T) {
			k := newKit(t)
			tamper(k)
			th := installHome(t, k)
			err := th.Install(context.Background())
			if err == nil || !strings.Contains(err.Error(), "does not match the pinned hash") {
				t.Fatalf("%v", err)
			}
			if _, e := os.Stat(th.runtimeDir()); e == nil {
				t.Error("a runtime folder exists after a failed install")
			}
			if _, ok := th.installedNow(); ok {
				t.Error("counts as installed")
			}
			ents, _ := os.ReadDir(th.root())
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "runtime") || strings.HasPrefix(e.Name(), "download-") {
					t.Errorf("left behind: %s", e.Name())
				}
			}
		})
	}
}

// The runtime download is the last: a bad Node stops it before the others are even asked for.
func TestInstallChecksEachDownloadBeforeTheNext(t *testing.T) {
	k := newKit(t)
	k.pin.Node.SHA256[testPlat] = sha256hex([]byte("other"))
	th := installHome(t, k)
	if err := th.Install(context.Background()); err == nil {
		t.Fatal("installed")
	}
	if len(k.asked) != 1 || !strings.Contains(k.asked[0], "nodejs.org") {
		t.Errorf("asked for %v after Node failed", k.asked)
	}
}

func TestInstallPackageMustBeT3AtThePinnedVersion(t *testing.T) {
	k := newKit(t)
	k.pkg = archive(t,
		tarEntry{name: "package/package.json", body: `{"name":"t3","version":"0.0.1"}`},
		tarEntry{name: "package/bin/t3.mjs", body: "// launcher\n"})
	k.setPin()
	th := installHome(t, k)
	if err := th.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "not the pinned one") {
		t.Fatalf("%v", err)
	}
}

func TestInstallRefusesAnArchiveThatLeavesItsFolder(t *testing.T) {
	top := "node-" + testNode + "-" + testPlat
	for name, e := range map[string]tarEntry{
		"dot-dot":        {name: top + "/../../escape.txt", body: "x"},
		"absolute":       {name: "/tmp/escape-t3-test.txt", body: "x"},
		"absolute link":  {name: top + "/bin/l", typ: tar.TypeSymlink, link: "/etc/passwd"},
		"link out":       {name: top + "/bin/l", typ: tar.TypeSymlink, link: "../../../../outside"},
		"hard link out":  {name: top + "/bin/h", typ: tar.TypeLink, link: "../../../etc/passwd"},
		"device":         {name: top + "/dev", typ: tar.TypeChar},
		"through a link": {name: top + "/lnk/file", body: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newKit(t)
			entries := []tarEntry{{name: top + "/bin/node", body: "x", mode: 0o755}}
			if name == "through a link" {
				// a link that points at the folder above, then a file written through it
				entries = append(entries, tarEntry{name: top + "/lnk", typ: tar.TypeSymlink, link: "../.."})
			}
			entries = append(entries, e)
			k.node = archive(t, entries...)
			k.setPin() // the hash is right: it is the content that is wrong
			th := installHome(t, k)
			if err := th.Install(context.Background()); err == nil {
				t.Fatal("an archive that leaves its folder was unpacked")
			}
			if _, err := os.Stat(th.runtimeDir()); err == nil {
				t.Error("a runtime folder exists")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(th.h.Dir), "escape.txt")); err == nil {
				t.Error("a file was written outside")
			}
			if _, err := os.Stat("/tmp/escape-t3-test.txt"); err == nil {
				t.Error("an absolute path was written")
			}
		})
	}
}

func TestInstallPlatformAndRunning(t *testing.T) {
	k := newKit(t)
	th := installHome(t, k)
	th.Platform = func() string { return "" }
	if err := th.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "no build for this computer") {
		t.Errorf("unsupported platform: %v", err)
	}
	th.Platform = func() string { return "other-plat" }
	if err := th.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "pins no Node or T3 download") {
		t.Errorf("platform with no pin: %v", err)
	}
	th.Platform = func() string { return testPlat }
	th.srv = &fakeServer{alive: true}
	if err := th.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "stop it before installing") {
		t.Errorf("install while running: %v", err)
	}
	if len(k.asked) != 0 {
		t.Errorf("downloaded %v for a refused install", k.asked)
	}
}

// Installing again replaces the folder and keeps working.
func TestInstallTwice(t *testing.T) {
	k := newKit(t)
	th := installHome(t, k)
	for range 2 {
		if err := th.Install(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, ok := th.installedNow(); !ok {
			t.Fatal("not installed")
		}
	}
	ents, _ := os.ReadDir(th.root())
	for _, e := range ents {
		if strings.Contains(e.Name(), ".old-") || strings.Contains(e.Name(), ".new-") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

func TestHTTPFetchIsHTTPSOnly(t *testing.T) {
	var buf bytes.Buffer
	if err := httpFetch(context.Background(), "http://example.com/x", &buf, 10); err == nil {
		t.Error("plain http was fetched")
	}
	if err := httpFetch(context.Background(), "file:///etc/passwd", &buf, 10); err == nil {
		t.Error("a file URL was fetched")
	}
}

func TestStripped(t *testing.T) {
	for in, want := range map[string]string{"a/b/c": "b/c", "a/": "", "a": "", "./a/b": "b", "a/./b": "b"} {
		if got, ok := stripped(in, 1); !ok || got != want {
			t.Errorf("stripped(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"/a/b", "../x", "a/../../x"} {
		if _, ok := stripped(in, 1); ok {
			t.Errorf("stripped(%q) accepted", in)
		}
	}
}

// ---- zips (Windows) ----

type zipEntry struct {
	name, body string
	mode       os.FileMode // 0 = a plain file; os.ModeDir, os.ModeSymlink, os.ModeDevice...
}

// zipArchive builds a .zip.
func zipArchive(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode&os.ModeType == 0 {
			mode |= 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if mode&os.ModeDir == 0 {
			if _, err := w.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const winPlat = "win32-x64"

// newWinKit is the kit of a Windows computer: Node and T3 are zips with node.exe and t3.exe, the npm package is still a tgz.
func newWinKit(t *testing.T) *installKit {
	t.Helper()
	k := newKit(t)
	k.plat = winPlat
	k.node = zipArchive(t,
		zipEntry{name: "node-" + testNode + "-win-x64/", mode: os.ModeDir},
		zipEntry{name: "node-" + testNode + "-win-x64/node.exe", body: "MZ node"},
		zipEntry{name: "node-" + testNode + "-win-x64/node_modules/npm/index.js", body: "//npm"})
	top := "t3-" + testVer + "-" + winPlat
	k.rt = zipArchive(t,
		zipEntry{name: top + "/", mode: os.ModeDir},
		zipEntry{name: top + "/t3.exe", body: "MZ t3"},
		zipEntry{name: top + "/client/index.html", body: "<html>"})
	k.setPin()
	return k
}

// On Windows the pins name zips, Node's own name for the platform is win-x64, the layout is node.exe and t3.exe, and the install
// is complete and recognised as the pinned one.
func TestInstallOnWindowsUnpacksZips(t *testing.T) {
	k := newWinKit(t)
	th := installHome(t, k)
	if err := th.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"https://nodejs.org/dist/" + testNode + "/node-" + testNode + "-win-x64.zip",
		"https://registry.npmjs.org/t3/-/t3-" + testVer + ".tgz",
		"https://github.com/pingdotgg/t3code/releases/download/v" + testVer + "/t3-" + testVer + "-win32-x64.zip"}
	if strings.Join(k.asked, "\n") != strings.Join(want, "\n") {
		t.Errorf("asked %q\nwant %q", k.asked, want)
	}
	for _, f := range []string{filepath.Join(th.runtimeDir(), "node", "node.exe"), th.exePath(testVer),
		filepath.Join(th.runtimeDir(), "pkg", "bin", "t3.mjs")} {
		if st, err := os.Stat(f); err != nil || !st.Mode().IsRegular() {
			t.Errorf("%s: %v", f, err)
		}
	}
	if filepath.Base(th.nodeBin()) != "node.exe" || filepath.Base(th.exePath(testVer)) != "t3.exe" {
		t.Errorf("paths: %s %s", th.nodeBin(), th.exePath(testVer))
	}
	if _, ok := th.installedNow(); !ok {
		t.Error("the install is not recognised as complete")
	}
	// the same kit on a Unix platform names tar.gz archives, node/bin/node and t3 (its tar holds a symlink, which a Windows
	// test machine can not make)
	if runtime.GOOS == "windows" {
		return
	}
	u := newKit(t)
	uh := installHome(t, u)
	if err := uh.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(uh.nodeBin()) != "node" || filepath.Base(uh.exePath(testVer)) != "t3" || !strings.HasSuffix(u.asked[0], ".tar.gz") {
		t.Errorf("unix: %s %s %v", uh.nodeBin(), uh.exePath(testVer), u.asked)
	}
}

// The zip is checked against its pinned hash before a byte of it is read, as the tar.gz is.
func TestInstallOnWindowsRefusesAZipThatDoesNotMatchThePin(t *testing.T) {
	for name, tamper := range map[string]func(k *installKit){
		"node": func(k *installKit) { k.node = zipArchive(t, zipEntry{name: "x/node.exe", body: "evil"}) },
		"t3":   func(k *installKit) { k.rt = zipArchive(t, zipEntry{name: "x/t3.exe", body: "evil"}) },
	} {
		t.Run(name, func(t *testing.T) {
			k := newWinKit(t)
			tamper(k) // the pin still names the original
			th := installHome(t, k)
			if err := th.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match the pinned hash") {
				t.Fatalf("%v", err)
			}
			if _, err := os.Stat(th.runtimeDir()); err == nil {
				t.Error("a runtime folder exists")
			}
		})
	}
}

// A zip that tries to leave its folder, write through a link, or bring in anything but folders and files is refused, and nothing
// is installed (the zip-slip cases of unpackTarGz, for zips).
func TestInstallOnWindowsRefusesAZipThatLeavesItsFolder(t *testing.T) {
	top := "node-" + testNode + "-win-x64"
	for name, e := range map[string]zipEntry{
		"dot-dot":        {name: top + "/../../escape.txt", body: "x"},
		"dot-dot first":  {name: "../escape.txt", body: "x"},
		"absolute":       {name: "/tmp/escape-t3-test.txt", body: "x"},
		"backslash":      {name: top + `\..\..\escape.txt`, body: "x"},
		"backslash only": {name: `..\escape.txt`, body: "x"},
		"drive":          {name: "C:/escape.txt", body: "x"},
		"drive in tree":  {name: top + "/C:evil", body: "x"},
		"stream":         {name: top + "/node.exe:hidden", body: "x"},
		"symlink":        {name: top + "/lnk", body: "../..", mode: os.ModeSymlink},
		"device":         {name: top + "/dev", mode: os.ModeDevice},
		"pipe":           {name: top + "/pipe", mode: os.ModeNamedPipe},
	} {
		t.Run(name, func(t *testing.T) {
			k := newWinKit(t)
			k.node = zipArchive(t, zipEntry{name: top + "/node.exe", body: "x"}, e)
			k.setPin() // the hash is right: it is the content that is wrong
			th := installHome(t, k)
			if err := th.Install(context.Background()); err == nil {
				t.Fatal("a zip that leaves its folder was unpacked")
			}
			if _, err := os.Stat(th.runtimeDir()); err == nil {
				t.Error("a runtime folder exists")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(th.h.Dir), "escape.txt")); err == nil {
				t.Error("a file was written outside")
			}
			if _, err := os.Stat("/tmp/escape-t3-test.txt"); err == nil {
				t.Error("an absolute path was written")
			}
		})
	}
	// a file written through a folder that an earlier entry made is fine; so is the strip of one component
	dest := t.TempDir()
	zp := filepath.Join(t.TempDir(), "ok.zip")
	if err := os.WriteFile(zp, zipArchive(t, zipEntry{name: "top/", mode: os.ModeDir}, zipEntry{name: "top/a/b.txt", body: "ok"}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unpackZip(zp, dest, 1); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "a", "b.txt")); err != nil || string(b) != "ok" {
		t.Errorf("%q %v", b, err)
	}
	// not a zip at all
	bad := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(bad, []byte("not a zip"), 0o600)
	if err := unpackZip(bad, t.TempDir(), 1); err == nil {
		t.Error("garbage was unpacked")
	}
}

// A plain zip unpacks all its files with the leading component dropped (Windows files carry no executable bit, and none is
// needed).
func TestUnpackZipKeepsEveryFile(t *testing.T) {
	zp := filepath.Join(t.TempDir(), "many.zip")
	var es []zipEntry
	for i := 0; i < 5; i++ {
		es = append(es, zipEntry{name: "t/f" + string(rune('a'+i)), body: "x"})
	}
	if err := os.WriteFile(zp, zipArchive(t, es...), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := unpackZip(zp, dest, 1); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dest)
	if len(ents) != 5 {
		t.Errorf("%d files", len(ents))
	}
}

// ---- platforms ----

// Every platform key: what T3 does there. The pins must name every platform that has a build, and Intel Macs are "no build" with
// the reason, before anything is downloaded.
func TestT3PlatformsAndWhatTheyNeed(t *testing.T) {
	pin, err := loadT3Pin()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"darwin-arm64", "linux-arm64", "linux-x64", "win32-x64"} {
		if why := t3Unavailable(pin, p); why != "" {
			t.Errorf("%s: %s", p, why)
		}
		if len(pin.Runtime.SHA256[p]) != 64 || len(pin.Node.SHA256[p]) != 64 {
			t.Errorf("%s: pins %q %q", p, pin.Runtime.SHA256[p], pin.Node.SHA256[p])
		}
	}
	if why := t3Unavailable(pin, "darwin-x64"); why != "T3 publishes no build for Intel Macs" {
		t.Errorf("darwin-x64: %q", why)
	}
	if why := t3Unavailable(pin, ""); !strings.Contains(why, "no build for this computer") {
		t.Errorf("no platform: %q", why)
	}
	if why := t3Unavailable(pin, "freebsd-x64"); !strings.Contains(why, "pins no T3 download") {
		t.Errorf("unpinned: %q", why)
	}
	// the names of the archives, per platform
	for plat, c := range map[string]struct{ node, t3 string }{
		"darwin-arm64": {"node-v1-darwin-arm64.tar.gz", "t3-9-darwin-arm64.tar.gz"},
		"linux-x64":    {"node-v1-linux-x64.tar.gz", "t3-9-linux-x64.tar.gz"},
		"win32-x64":    {"node-v1-win-x64.zip", "t3-9-win32-x64.zip"},
	} {
		if nodeArchiveName("v1", plat) != c.node || t3ArchiveName("9", plat) != c.t3 {
			t.Errorf("%s: %s %s", plat, nodeArchiveName("v1", plat), t3ArchiveName("9", plat))
		}
		// and they are the names t3bump looks the sums of up
		if t3ArchiveName("9", plat) != t3bump.ArchiveName("9", plat) || nodeArchiveName("v1", plat) != t3bump.NodeArchive("v1", plat) {
			t.Errorf("%s: t3bump names %s %s", plat, t3bump.ArchiveName("9", plat), t3bump.NodeArchive("v1", plat))
		}
	}
	// the bump tool and the installer agree on which platforms are pinned and which have no build
	pinned := map[string]bool{}
	for _, p := range t3bump.Platforms {
		pinned[p] = true
		if t3Unavailable(pin, p) != "" {
			t.Errorf("t3bump pins %s but Home can't install on it", p)
		}
	}
	for p := range pin.Runtime.SHA256 {
		if !pinned[p] {
			t.Errorf("upstream.json pins %s, which t3bump does not keep up", p)
		}
	}
	if !reflect.DeepEqual(t3bump.NoBuild, t3NoBuild) {
		t.Errorf("no-build lists differ: %v %v", t3bump.NoBuild, t3NoBuild)
	}
}

// An Intel Mac is told so before any download, in the gate's words; and Windows and Intel Mac keys are what the system gives.
func TestInstallOnAPlatformWithNoUpstreamBuild(t *testing.T) {
	k := newKit(t)
	k.plat = "darwin-x64"
	k.setPin()
	th := installHome(t, k)
	err := th.Install(context.Background())
	if err == nil || err.Error() != "That isn't available on this computer (T3 publishes no build for Intel Macs)." {
		t.Fatalf("%v", err)
	}
	if len(k.asked) != 0 {
		t.Errorf("downloaded %v", k.asked)
	}
	st := th.Status()
	if st["supported"] != false || st["unsupported_reason"] != "T3 publishes no build for Intel Macs" {
		t.Errorf("status: %v %v", st["supported"], st["unsupported_reason"])
	}
}
