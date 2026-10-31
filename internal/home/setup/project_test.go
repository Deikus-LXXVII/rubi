package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func chooser(path string) func(argv []string) ([]byte, error, bool) {
	return func(argv []string) ([]byte, error, bool) {
		if argv[0] == "/usr/bin/osascript" {
			return []byte(path + "\n"), nil, true
		}
		return nil, nil, false
	}
}

func TestProjectAdd(t *testing.T) {
	e := newTestEnv(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(dir, "my-project")
	os.Mkdir(proj, 0o755)
	e.m.extra = chooser(proj + "/")
	res, err := e.op("project.add", nil)
	if err != nil || res["added"] != proj {
		t.Fatalf("%v %v", res, err)
	}
	if len(e.cfg.Projects) != 1 || e.cfg.Projects[0] != proj {
		t.Errorf("config: %v", e.cfg.Projects)
	}
	var argv []string
	for _, c := range e.cmd.calls {
		argv = c
	}
	if argv[0] != "/usr/bin/osascript" || argv[1] != "-e" || !strings.Contains(argv[2], "POSIX path of (choose folder") {
		t.Errorf("chooser argv: %v", argv)
	}
	if _, err := e.op("project.add", nil); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("same folder twice: %v", err)
	}
	if len(e.cfg.Projects) != 1 {
		t.Errorf("config: %v", e.cfg.Projects)
	}
}

func TestProjectAddCancelled(t *testing.T) {
	e := newTestEnv(t)
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		return []byte("0:59: execution error: User canceled. (-128)"), os.ErrInvalid, true
	}
	res, err := e.op("project.add", nil)
	if err != nil || res["cancelled"] != true || len(e.cfg.Projects) != 0 {
		t.Errorf("%v %v %v", res, err, e.cfg.Projects)
	}
	e.m.extra = func(argv []string) ([]byte, error, bool) { return []byte("boom"), os.ErrInvalid, true }
	if _, err := e.op("project.add", nil); err == nil {
		t.Error("a failing chooser isn't an error")
	}
	e.s.opt.GOOS = "linux"
	if _, err := e.op("project.add", nil); err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Errorf("not a Mac: %v", err)
	}
}

func TestProjectAddRefusals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	realHome, _ := filepath.EvalSymlinks(home)
	for _, d := range []string{".ssh", "Library/Caches", "work/app"} {
		os.MkdirAll(filepath.Join(home, d), 0o755)
	}
	e := newTestEnv(t, func(o *Options) { o.SystemTrees = []string{filepath.Join(realHome, "work", "system")} })
	os.MkdirAll(filepath.Join(realHome, "work", "system", "inner"), 0o755)
	file := filepath.Join(realHome, "work", "file")
	os.WriteFile(file, []byte("x"), 0o644)
	inside := filepath.Join(e.dir, "sub")
	os.MkdirAll(inside, 0o755)
	link := filepath.Join(realHome, "work", "link")
	os.Symlink(e.dir, link)
	for name, c := range map[string]string{
		"home itself":        realHome,
		"parent of home":     filepath.Dir(realHome),
		"root":               "/",
		"private folder":     filepath.Join(realHome, ".ssh"),
		"Library":            filepath.Join(realHome, "Library", "Caches"),
		"system tree":        filepath.Join(realHome, "work", "system", "inner"),
		"Rubi Home's folder": e.dir,
		"inside Rubi Home":   inside,
		"link to Rubi Home":  link,
		"a file":             file,
		"missing":            filepath.Join(realHome, "nope"),
		"relative":           "work/app",
		"Volumes itself":     "/Volumes",
		"comma in the path":  "",
	} {
		if name == "comma in the path" {
			d := filepath.Join(realHome, "work", "a,b")
			os.MkdirAll(d, 0o755)
			c = d
		}
		e.cfg.Projects = nil
		e.m.extra = chooser(c)
		if res, err := e.op("project.add", nil); err == nil || len(e.cfg.Projects) != 0 {
			t.Errorf("%s accepted: %v %v", name, res, e.cfg.Projects)
		}
	}
	// a folder in the home is fine
	e.m.extra = chooser(filepath.Join(realHome, "work", "app"))
	if res, err := e.op("project.add", nil); err != nil || res["added"] != filepath.Join(realHome, "work", "app") {
		t.Errorf("fine folder: %v %v", res, err)
	}
}

func TestProjectAddLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < MaxProjects; i++ {
		e.cfg.Projects = append(e.cfg.Projects, filepath.Join("/x", string(rune('a'+i))))
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	e.m.extra = chooser(dir)
	if _, err := e.op("project.add", nil); err == nil || !strings.Contains(err.Error(), "20") {
		t.Errorf("21st project: %v", err)
	}
	if len(e.cfg.Projects) != MaxProjects {
		t.Error("config changed")
	}
	if e.cmd.ran("/usr/bin/osascript") != 0 {
		t.Error("the chooser was shown although no project can be added")
	}
}

func TestProjectRemove(t *testing.T) {
	e := newTestEnv(t)
	a := e.addProject("a")
	b := e.addProject("b")
	e.s.mu.Lock()
	e.s.tests[a.Dir] = result{outcome: outcome{Done, "ok"}}
	e.s.mu.Unlock()
	if _, err := e.op("project.remove", map[string]any{"dir": "/nowhere"}); err == nil {
		t.Error("removed a folder that isn't a project")
	}
	if res, err := e.op("project.remove", map[string]any{"dir": a.Dir}); err != nil || res["removed"] != true {
		t.Fatalf("%v %v", res, err)
	}
	if len(e.cfg.Projects) != 1 || e.cfg.Projects[0] != b.Dir {
		t.Errorf("config: %v", e.cfg.Projects)
	}
	if _, ok := e.s.tests[a.Dir]; ok {
		t.Error("the self-test result of a removed project is kept")
	}
	// the folder itself is untouched
	if _, err := os.Stat(a.Dir); err != nil {
		t.Error(err)
	}
	// not while a job works on it
	e.ready()
	e.ag.token = true
	e.ag.box.probeOut = goodProbe
	e.ag.selftest = &fakeSelfTest{box: e.ag.box, turn: goodTurn, hold: make(chan struct{})}
	if _, err := e.op("run", map[string]any{"step": "selftest", "project": b.ID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job", func() bool { return e.s.Status(context.Background()).Job != nil })
	if _, err := e.op("project.remove", map[string]any{"dir": b.Dir}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
		t.Errorf("removed during a job: %v", err)
	}
	e.s.Cancel()
	e.wait()
}

func TestSharedPlaces(t *testing.T) {
	const home = "/Users/alice"
	for path, refused := range map[string]bool{
		"/Volumes/Backup":            true,
		"/Volumes/Backup/my-project": false,
		"/Volumes":                   false, // refused elsewhere (systemExact)
		"/Users/Shared":              true,
		"/Users/Shared/my-project":   true,
		"/Users/bob":                 true,
		"/Users/bob/my-project":      true,
		"/Users/alice/my-project":    false,
		"/Users/alice2/my-project":   true,
		"/Users":                     false,
		"/srv/my-project":            false,
	} {
		if got := sharedPlace(path, home) != ""; got != refused {
			t.Errorf("%s: refused=%v, want %v", path, got, refused)
		}
	}
}
