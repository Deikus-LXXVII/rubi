package home

import (
	"archive/tar"
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
	"strings"
	"sync"
	"testing"
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
	pin           t3Pin
	mu            sync.Mutex
	asked         []string
}

func newKit(t *testing.T) *installKit {
	t.Helper()
	k := &installKit{}
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
	k.pin.Runtime.SHA256 = map[string]string{testPlat: sha256hex(k.rt)}
	k.pin.Node.Version = testNode
	k.pin.Node.SHA256 = map[string]string{testPlat: sha256hex(k.node)}
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
	h.T3.Fetch, h.T3.Pin, h.T3.Platform = k.fetch, &k.pin, func() string { return testPlat }
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
