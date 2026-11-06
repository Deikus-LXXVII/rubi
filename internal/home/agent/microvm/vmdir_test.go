//go:build unix

package microvm

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func mode(t *testing.T, p string) os.FileMode {
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestPrepareDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "web-app")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("made with %v", m)
	}
	// a second start finds it open to others and full of the last run's files: closed again, ours removed, the rest kept
	_ = os.Chmod(dir, 0o755)
	for _, n := range []string{"control.sock", "console.sock", "qemu.pid", "fs0.sock", "fs0.pid", "fs12.sock.pid", "fs3.pid.tmp", "notes.txt"} {
		_ = os.WriteFile(filepath.Join(dir, n), nil, 0o600)
	}
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("left with %v", m)
	}
	ents, _ := os.ReadDir(dir)
	var left []string
	for _, e := range ents {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"notes.txt"}) {
		t.Fatalf("left %q", left)
	}
}

func TestPrepareDirRefuses(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(root, "elsewhere")
	_ = os.Mkdir(elsewhere, 0o755)
	link := filepath.Join(root, "link")
	_ = os.Symlink(elsewhere, link)
	file := filepath.Join(root, "file")
	_ = os.WriteFile(file, nil, 0o600)
	for name, dir := range map[string]string{
		"a symbolic link":     link,
		"a file":              file,
		"a relative path":     "vm/web",
		"no parent":           filepath.Join(root, "no", "such"),
		"an unclean path":     root + "/x/../link",
		"a newline in a path": root + "/a\nb",
	} {
		if err := PrepareDir(dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if m := mode(t, elsewhere); m != 0o755 {
		t.Errorf("the link's target was changed to %v", m)
	}
}

func TestPidFiles(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "fs0.pid")
	if err := writePid(p, 4242); err != nil {
		t.Fatal(err)
	}
	if pid, err := readPid(p); pid != 4242 || err != nil {
		t.Fatalf("%d %v", pid, err)
	}
	if _, err := os.Stat(p + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp left: %v", err)
	}
	for _, bad := range []string{"", "1", "0", "-5", "abc", "12 13"} {
		_ = os.WriteFile(p, []byte(bad), 0o600)
		if pid, err := readPid(p); err == nil {
			t.Errorf("%q read as %d", bad, pid)
		}
	}
}

// The sweep kills only a pid whose process still names its VM's folder, clears the folders of their files and keeps
// what isn't the launcher's.
func TestSweep(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		_ = os.Mkdir(d, 0o700)
	}
	files := map[string]string{
		a + "/qemu.pid":     "111",
		a + "/fs0.pid":      "222", // reused by another program
		a + "/fs1.pid":      "garbage",
		a + "/control.sock": "",
		b + "/fs0.pid":      "333",
		b + "/notes.pid":    "444", // not the launcher's
		b + "/keep.txt":     "",
	}
	for p, c := range files {
		_ = os.WriteFile(p, []byte(c), 0o600)
	}
	_ = os.WriteFile(filepath.Join(root, "stray.pid"), []byte("555"), 0o600) // not in a VM folder
	ours := map[int]string{111: a, 333: b, 444: b, 555: root}
	var killed []int
	err := Sweeper{
		Root: root,
		Ours: func(pid int, dir string) bool { return ours[pid] == dir },
		Kill: func(pid int) error { killed = append(killed, pid); return nil },
	}.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(killed)
	if !slices.Equal(killed, []int{111, 333}) {
		t.Fatalf("killed %v", killed)
	}
	for p := range files {
		_, err := os.Stat(p)
		kept := filepath.Base(p) == "notes.pid" || filepath.Base(p) == "keep.txt"
		if kept != (err == nil) {
			t.Errorf("%s: kept %v", p, err == nil)
		}
	}
	if err := (Sweeper{Root: filepath.Join(root, "none")}).Sweep(); err != nil {
		t.Errorf("no root: %v", err)
	}
}

func TestCmdlineNames(t *testing.T) {
	dir := "/home/u/.local/state/rubi-home/vm/web"
	for cmdline, want := range map[string]bool{
		"/opt/qemu\x00-pidfile\x00/home/u/.local/state/rubi-home/vm/web/qemu.pid":                     true,
		"virtiofsd\x00--socket-path\x00/home/u/.local/state/rubi-home/vm/web/fs0.sock":                true,
		"qemu\x00-chardev\x00socket,id=c,path=/home/u/.local/state/rubi-home/vm/web/c.sock,server=on": true,
		"qemu\x00-pidfile\x00/home/u/.local/state/rubi-home/vm/web2/qemu.pid":                         false,
		"bash\x00/home/u/.local/state/rubi-home/vm/web":                                               false,
		"": false,
	} {
		if got := cmdlineNames([]byte(cmdline), dir); got != want {
			t.Errorf("%q: %v", cmdline, got)
		}
	}
	if !cmdlineNames([]byte("qemu\x00path=/vm/a,,b/c.sock,server=on"), "/vm/a,b") {
		t.Error("a comma doubled in an option list")
	}
}
