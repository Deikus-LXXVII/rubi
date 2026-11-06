package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectAdd(t *testing.T) {
	e := newTestEnv(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	proj := filepath.Join(dir, "my-project")
	os.Mkdir(proj, 0o755)
	res, err := e.op("project.add", map[string]any{"dir": proj})
	if err != nil || res["added"] != proj {
		t.Fatalf("%v %v", res, err)
	}
	if len(e.cfg.Projects) != 1 || e.cfg.Projects[0] != proj {
		t.Errorf("config: %v", e.cfg.Projects)
	}
	if _, err := e.op("project.add", map[string]any{"dir": proj}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("same folder twice: %v", err)
	}
	if len(e.cfg.Projects) != 1 {
		t.Errorf("config: %v", e.cfg.Projects)
	}
}

// Rubi Home shows no folder chooser on any system: a project.add that names no folder is refused, and no program
// is run to ask for one.
func TestProjectAddWithoutAFolderShowsNoChooser(t *testing.T) {
	e := newTestEnv(t)
	for _, args := range []map[string]any{nil, {}, {"dir": ""}} {
		if res, err := e.op("project.add", args); err == nil || !strings.Contains(err.Error(), "Rubi panel") || len(e.cfg.Projects) != 0 {
			t.Errorf("%v: %v %v %v", args, res, err, e.cfg.Projects)
		}
	}
	if len(e.cmd.calls) != 0 {
		t.Errorf("a program was run: %v", e.cmd.calls)
	}
	e.s.opt.Backend = noBackendLinux
	if _, err := e.op("project.add", map[string]any{"dir": t.TempDir()}); err == nil || !strings.Contains(err.Error(), "agents") {
		t.Errorf("not a Mac: %v", err)
	}
}

func TestProjectAddRefusals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	realHome, _ := filepath.EvalSymlinks(home)
	// a folder of the home that is private on this system (hostpaths): Library on a Mac, .config on Linux
	private := filepath.Join("Library", "Caches")
	if runtime.GOOS != "darwin" {
		private = filepath.Join(".config", "x")
	}
	for _, d := range []string{".ssh", private, "work/app"} {
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
		"private on this OS": filepath.Join(realHome, private),
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
		if c != "" {
			if res, err := e.op("project.add", map[string]any{"dir": c}); err == nil || len(e.cfg.Projects) != 0 {
				t.Errorf("%s accepted: %v %v", name, res, e.cfg.Projects)
			}
		}
	}
	// a folder in the home is fine
	e.cfg.Projects = nil
	e.cmd.calls = nil
	if res, err := e.op("project.add", map[string]any{"dir": filepath.Join(realHome, "work", "app")}); err != nil || res["added"] != filepath.Join(realHome, "work", "app") {
		t.Errorf("fine folder as dir: %v %v", res, err)
	}
	if len(e.cmd.calls) != 0 {
		t.Errorf("a program was run: %v", e.cmd.calls)
	}
}

func TestProjectAddLimit(t *testing.T) {
	e := newTestEnv(t)
	for i := 0; i < MaxProjects; i++ {
		e.cfg.Projects = append(e.cfg.Projects, filepath.Join("/x", string(rune('a'+i))))
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if _, err := e.op("project.add", map[string]any{"dir": dir}); err == nil || !strings.Contains(err.Error(), "20") {
		t.Errorf("21st project: %v", err)
	}
	if len(e.cfg.Projects) != MaxProjects {
		t.Error("config changed")
	}
}

func TestProjectRemove(t *testing.T) {
	e := newTestEnv(t)
	a := e.addProject("a")
	b := e.addProject("b")
	if _, err := e.op("project.remove", map[string]any{"dir": "/nowhere"}); err == nil {
		t.Error("removed a folder that isn't a project")
	}
	if res, err := e.op("project.remove", map[string]any{"dir": a.Dir}); err != nil || res["removed"] != true {
		t.Fatalf("%v %v", res, err)
	}
	if len(e.cfg.Projects) != 1 || e.cfg.Projects[0] != b.Dir {
		t.Errorf("config: %v", e.cfg.Projects)
	}
	// the folder itself is untouched
	if _, err := os.Stat(a.Dir); err != nil {
		t.Error(err)
	}
	// not while a job works on it
	j, _, err := e.s.begin("all", b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.op("project.remove", map[string]any{"dir": b.Dir}); err == nil || !strings.HasPrefix(err.Error(), "busy") {
		t.Errorf("removed during a job: %v", err)
	}
	e.s.release(j)
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

func TestSharedPlaceIgnoresCase(t *testing.T) {
	if sharedPlace("/users/ALICE/x", "/Users/bob") == "" {
		t.Error("another user's folder in other case was allowed")
	}
	if sharedPlace("/users/BOB/x", "/Users/bob") != "" {
		t.Error("the user's own folder in other case was refused")
	}
	if sharedPlace("/VOLUMES/Disk", "/Users/bob") == "" {
		t.Error("a disk root in other case was allowed")
	}
}

func TestProjectAddSystemFoldersByCaseAndLink(t *testing.T) {
	e := newTestEnv(t, func(o *Options) { o.SystemTrees = nil })
	link := filepath.Join(t.TempDir(), "etc-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatal(err)
	}
	cands := []string{link}
	if _, err := os.Stat("/SYSTEM"); err == nil { // case-insensitive file system
		cands = append(cands, "/SYSTEM", "/users/ALICE")
	}
	for _, c := range cands {
		if _, err := e.s.refuseFolder(c, nil); err == nil {
			t.Errorf("%s accepted", c)
		}
	}
}
