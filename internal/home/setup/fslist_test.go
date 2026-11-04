package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// folderEnv is a Setup whose user has a home folder with some folders in it. Rubi Home's own folder is inside it,
// as it is on a real computer, and no system tree applies (the machine's own are unknown here).
func folderEnv(t *testing.T, mods ...func(*Options)) (*testEnv, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	real, _ := filepath.EvalSymlinks(home)
	e := newTestEnv(t, append([]func(*Options){func(o *Options) {
		o.Dir = filepath.Join(real, "helper-data")
		o.BrowseRoots = func() []string { return nil } // no disk: the machine's own are not the test's business
	}}, mods...)...)
	if err := os.MkdirAll(e.s.opt.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return e, real
}

func mk(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func names(l *FolderList) []string {
	var out []string
	for _, f := range l.Folders {
		out = append(out, f.Name)
	}
	return out
}

func TestFoldersListsHomeFoldersOnly(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "Zeta"), filepath.Join(home, "alpha", "inner"), filepath.Join(home, "Beta"), filepath.Join(home, ".hidden"))
	secret := filepath.Join(home, "secret-notes.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET CONTENT"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(l), ","); got != "alpha,Beta,Zeta" {
		t.Fatalf("folders %q: sorted, folders only, no dot folder, no helper folder, no file", got)
	}
	if l.Path != home || l.Parent != "" || l.CanChoose || l.Reason == "" {
		t.Errorf("the home folder itself: path %q parent %q choose %v (%q)", l.Path, l.Parent, l.CanChoose, l.Reason)
	}
	if len(l.Places) == 0 || l.Places[0].Name != "Home" || l.Places[0].Path != home {
		t.Errorf("places %+v", l.Places)
	}
	raw, _ := json.Marshal(l)
	for _, bad := range []string{"secret-notes", "TOP SECRET", ".hidden", "helper-data"} {
		if strings.Contains(string(raw), bad) {
			t.Errorf("the answer holds %q: %s", bad, raw)
		}
	}
	for _, f := range l.Folders {
		if !f.CanChoose || f.Reason != "" || !filepath.IsAbs(f.Path) {
			t.Errorf("%+v", f)
		}
	}
	// into a folder: it can be chosen, and its parent is the home folder
	sub, err := e.s.ListFolders(filepath.Join(home, "alpha"))
	if err != nil || !sub.CanChoose || sub.Parent != home || strings.Join(names(sub), ",") != "inner" {
		t.Fatalf("%+v %v", sub, err)
	}
}

// Nothing above the home folder (or a mounted disk) can be shown, and neither can a path that is not a folder.
func TestFoldersRefusesWhatIsOutsideOrNotAFolder(t *testing.T) {
	e, home := folderEnv(t)
	file := filepath.Join(home, "f.txt")
	os.WriteFile(file, []byte("x"), 0o600)
	long := "/" + strings.Repeat("a", maxFolderPath)
	for name, p := range map[string]string{
		"parent of home": filepath.Dir(home), "the root": string(filepath.Separator), "a file": file, "missing": filepath.Join(home, "nope"),
		"relative": "work/app", "NUL": home + "\x00/x", "newline": home + "\n", "too long": long,
	} {
		if l, err := e.s.ListFolders(p); err == nil {
			t.Errorf("%s was listed: %+v", name, l)
		}
	}
	// a sibling of the home folder is outside every root
	other := t.TempDir()
	if l, err := e.s.ListFolders(other); err == nil && !strings.HasPrefix(other, home) {
		t.Errorf("a folder outside the home folder was listed: %+v", l)
	}
}

// The deny lists are not shown, and cannot be listed even by naming them: what hostpaths names as private in the
// home, a system tree, and Rubi Home's own folder.
func TestFoldersHidesAndRefusesThePrivateAndTheSystem(t *testing.T) {
	system := t.TempDir()
	e, home := folderEnv(t)
	sysReal, _ := filepath.EvalSymlinks(system)
	e.s.opt.SystemTrees = []string{filepath.Join(sysReal, "tree")}
	var denied []string
	for _, d := range append(hostpaths.SensitiveHomeDirs(), hostpaths.DeniedTrees()...) {
		mk(t, filepath.Join(home, d, "deep"))
		denied = append(denied, filepath.Join(home, d), filepath.Join(home, d, "deep"))
	}
	mk(t, filepath.Join(home, "work"), filepath.Join(e.s.opt.Dir, "inner"))
	denied = append(denied, e.s.opt.Dir, filepath.Join(e.s.opt.Dir, "inner"))
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names(l), ",") != "work" {
		t.Fatalf("listed %v: the private folders and Rubi Home's own must not show", names(l))
	}
	for _, d := range denied {
		if l, err := e.s.ListFolders(d); err == nil {
			t.Errorf("%s was listed: %+v", d, l)
		}
	}
	// a system tree inside a root is refused too, and its parent does not show it
	mk(t, filepath.Join(home, "sys"))
	e.s.opt.SystemTrees = []string{filepath.Join(home, "sys")}
	if l, err := e.s.ListFolders(filepath.Join(home, "sys")); err == nil {
		t.Errorf("a system tree was listed: %+v", l)
	}
	l, _ = e.s.ListFolders("")
	for _, n := range names(l) {
		if n == "sys" {
			t.Error("a system tree shows in its parent")
		}
	}
}

// A link is shown only if it leads to a folder in the same root that is not denied: not out of the root, not into a
// private folder or Rubi Home's, not to a file, not nowhere.
func TestFoldersLinksCannotLeadIntoDeniedPlaces(t *testing.T) {
	e, home := folderEnv(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	mk(t, filepath.Join(home, "real"), filepath.Join(outside, "elsewhere"))
	private := filepath.Join(home, hostpaths.DeniedTrees()[0])
	mk(t, private)
	os.WriteFile(filepath.Join(home, "afile"), []byte("x"), 0o600)
	links := map[string]string{
		"ok":       filepath.Join(home, "real"),
		"toprivat": private,
		"tohelper": e.s.opt.Dir,
		"toout":    filepath.Join(outside, "elsewhere"),
		"tofile":   filepath.Join(home, "afile"),
		"nowhere":  filepath.Join(home, "missing"),
		"toroot":   string(filepath.Separator),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(home, name)); err != nil {
			t.Skip("no symbolic links here:", err)
		}
	}
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(l), ","); got != "ok,real" {
		t.Fatalf("listed %q: only the link to a folder of the home folder", got)
	}
	for _, f := range l.Folders {
		if f.Name == "ok" && f.Path != filepath.Join(home, "real") {
			t.Errorf("a link is listed with the path it leads to: %q", f.Path)
		}
	}
	// and naming a link into a denied place gets the same answer as naming the place
	if l, err := e.s.ListFolders(filepath.Join(home, "toprivat")); err == nil {
		t.Errorf("a link into a private folder was listed: %+v", l)
	}
	if l, err := e.s.ListFolders(filepath.Join(home, "toout")); err == nil {
		t.Errorf("a link out of the home folder was listed: %+v", l)
	}
}

func TestFoldersSaysWhyAFolderCannotBeChosen(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "taken"), filepath.Join(home, "free"))
	e.cfg.Projects = []string{filepath.Join(home, "taken")}
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range l.Folders {
		switch f.Name {
		case "taken":
			if f.CanChoose || !strings.Contains(f.Reason, "already") {
				t.Errorf("%+v", f)
			}
		case "free":
			if !f.CanChoose || f.Reason != "" {
				t.Errorf("%+v", f)
			}
		}
	}
	// the listing never decides for project.add: a folder it calls free is checked again there, and the other way round
	if _, err := e.s.AddProjectDir(filepath.Join(home, "taken")); err == nil {
		t.Error("project.add accepted a folder the listing refused")
	}
	if _, err := e.s.AddProjectDir(filepath.Join(home, "free")); err != nil {
		t.Errorf("project.add refused a folder the listing offered: %v", err)
	}
}

func TestFoldersBoundsTheAnswerAndSkipsOddNames(t *testing.T) {
	e, home := folderEnv(t)
	for i := 0; i < maxFolderEntries+7; i++ {
		mk(t, filepath.Join(home, "d"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)))
	}
	l, err := e.s.ListFolders("")
	if err != nil || len(l.Folders) != maxFolderEntries || !l.Truncated {
		t.Fatalf("%d folders, truncated %v, %v", len(l.Folders), l.Truncated, err)
	}
	// everything was sorted before the cut: the first 500 by name, not the first 500 the folder happened to give
	if l.Folders[0].Name != "d0000" || l.Folders[maxFolderEntries-1].Name != "d0499" {
		t.Errorf("the cut is not by name: %s .. %s", l.Folders[0].Name, l.Folders[maxFolderEntries-1].Name)
	}
	if err := os.Mkdir(filepath.Join(home, "bad\nname"), 0o755); err == nil {
		l2, _ := e.s.ListFolders("")
		for _, f := range l2.Folders {
			if strings.ContainsAny(f.Name, "\n") {
				t.Errorf("a name with a newline was listed: %q", f.Name)
			}
		}
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// The op that Rubi Home answers is the listing, with a path argument, and bad arguments are refused.
func TestFoldersOp(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "proj"))
	l, err := e.s.ListFolders(home)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(l)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"path", "name", "parent", "can_choose", "places", "folders"} {
		if _, ok := back[k]; !ok {
			t.Errorf("the answer has no %q: %s", k, raw)
		}
	}
	for _, f := range back["folders"].([]any) {
		for k := range f.(map[string]any) {
			if k != "name" && k != "path" && k != "can_choose" && k != "reason" {
				t.Errorf("a folder entry has %q: only names and paths go to the panel", k)
			}
		}
	}
}

// Truncated is true when, and only when, there is a folder more than the answer holds.
func TestFoldersTruncatedIsHonest(t *testing.T) {
	e, home := folderEnv(t)
	for i := 0; i < maxFolderEntries; i++ {
		mk(t, filepath.Join(home, "d"+strings.Repeat("0", 4-len(itoa(i)))+itoa(i)))
	}
	l, err := e.s.ListFolders("")
	if err != nil || len(l.Folders) != maxFolderEntries || l.Truncated {
		t.Fatalf("exactly %d folders: %d, truncated %v, %v", maxFolderEntries, len(l.Folders), l.Truncated, err)
	}
	// files and hidden folders are not "more folders"
	os.WriteFile(filepath.Join(home, "zfile"), nil, 0o600)
	mk(t, filepath.Join(home, ".zhidden"))
	if l, _ = e.s.ListFolders(""); l.Truncated {
		t.Error("a file or a hidden folder made the list truncated")
	}
	mk(t, filepath.Join(home, "d0500"))
	if l, _ = e.s.ListFolders(""); !l.Truncated || len(l.Folders) != maxFolderEntries {
		t.Errorf("one more folder: %d, truncated %v", len(l.Folders), l.Truncated)
	}
}

// A mounted disk is a root of its own: it can be listed, never chosen as a whole, and nothing above it can be reached.
func TestFoldersADiskIsARootButNotAProject(t *testing.T) {
	disk, _ := filepath.EvalSymlinks(t.TempDir())
	mk(t, filepath.Join(disk, "work", "app"))
	e, home := folderEnv(t, func(o *Options) { o.BrowseRoots = func() []string { return []string{disk} } })
	l, err := e.s.ListFolders(disk)
	if err != nil {
		t.Fatal(err)
	}
	if l.Parent != "" || l.CanChoose || !strings.Contains(l.Reason, "disk") || strings.Join(names(l), ",") != "work" {
		t.Fatalf("the disk: parent %q choose %v reason %q folders %v", l.Parent, l.CanChoose, l.Reason, names(l))
	}
	if len(l.Places) != 2 || l.Places[0].Path != home || l.Places[1].Path != disk {
		t.Errorf("places %+v", l.Places)
	}
	sub, err := e.s.ListFolders(filepath.Join(disk, "work"))
	if err != nil || !sub.CanChoose || sub.Parent != disk {
		t.Fatalf("a folder on the disk: %+v %v", sub, err)
	}
	if _, err := e.s.refuseFolder(disk, nil); err == nil || !strings.Contains(err.Error(), "disk") {
		t.Errorf("the disk's root as a project: %v", err)
	}
	if _, err := e.s.AddProjectDir(disk); err == nil {
		t.Error("project.add took a disk's root")
	}
	if _, err := e.s.refuseFolder(filepath.Join(disk, "work", "app"), nil); err != nil {
		t.Errorf("a folder on the disk: %v", err)
	}
	// the folder that holds the disks, and a sibling, are not roots
	for _, p := range []string{filepath.Dir(disk), filepath.Join(filepath.Dir(disk), "other")} {
		os.MkdirAll(p, 0o755)
		if l, err := e.s.ListFolders(p); err == nil && !strings.HasPrefix(p, home) {
			t.Errorf("%s was listed: %+v", p, l)
		}
	}
}

// No more than maxFolderDepth levels below a root: the folder at that depth is listed without its sub-folders, and
// the one below it can not be reached.
func TestFoldersDepthIsCapped(t *testing.T) {
	e, home := folderEnv(t)
	p := home
	var paths []string
	for i := 1; i <= maxFolderDepth+2; i++ {
		p = filepath.Join(p, "l"+itoa(i))
		paths = append(paths, p)
	}
	mk(t, p)
	for i, q := range paths {
		depth := i + 1
		l, err := e.s.ListFolders(q)
		switch {
		case depth < maxFolderDepth:
			if err != nil || l.Deep || len(l.Folders) != 1 {
				t.Errorf("depth %d: %+v %v", depth, l, err)
			}
		case depth == maxFolderDepth:
			if err != nil || !l.Deep || len(l.Folders) != 0 || !l.CanChoose {
				t.Errorf("depth %d: %+v %v", depth, l, err)
			}
		default:
			if err == nil {
				t.Errorf("depth %d was listed", depth)
			}
		}
	}
	if depthOf(home, home) != 0 || depthOf(filepath.Join(home, "a", "b"), home) != 2 {
		t.Error("depthOf")
	}
}

// Another user's home is never listed: not as a folder of Users or home on a disk, not by name, whatever the case.
func TestFoldersHideOtherUsersHomes(t *testing.T) {
	me := currentUser()
	if me == "" {
		t.Skip("no user name")
	}
	disk, _ := filepath.EvalSymlinks(t.TempDir())
	for _, c := range []string{"Users", "home", "USERS"} {
		mk(t, filepath.Join(disk, c, me, "mine"), filepath.Join(disk, c, "bob", "his-secret-project"), filepath.Join(disk, c, "Shared"))
	}
	mk(t, filepath.Join(disk, "work"))
	e, _ := folderEnv(t, func(o *Options) { o.BrowseRoots = func() []string { return []string{disk} } })
	top, err := e.s.ListFolders(disk)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(top), ","); !strings.Contains(got, "Users") || !strings.Contains(got, "work") {
		t.Fatalf("the disk's folders %q", got)
	}
	for _, c := range []string{"Users", "home", "USERS"} {
		l, err := e.s.ListFolders(filepath.Join(disk, c))
		if err != nil || strings.Join(names(l), ",") != me {
			t.Errorf("%s lists %v (%v): only %q", c, names(l), err, me)
		}
		if l, err := e.s.ListFolders(filepath.Join(disk, c, me)); err != nil || strings.Join(names(l), ",") != "mine" {
			t.Errorf("my own folder in %s: %v %v", c, names(l), err)
		}
		for _, other := range []string{"bob", "Shared", "bob/his-secret-project"} {
			if l, err := e.s.ListFolders(filepath.Join(disk, c, other)); err == nil {
				t.Errorf("%s/%s was listed: %+v", c, other, l)
			}
		}
	}
	raw, _ := json.Marshal(top)
	if strings.Contains(string(raw), "his-secret-project") {
		t.Error("another user's folder name is in the answer")
	}
	// otherUsersHome on its own
	for p, want := range map[string]bool{
		"/d/Users/bob": true, "/d/Users/" + me: false, "/d/Users/" + me + "/x": false, "/d/home/bob/x": true, "/d/work/Users": false,
		"/d/Users": false, "/d/work/home/bob": true,
	} {
		if got := otherUsersHome(filepath.FromSlash(p), filepath.FromSlash("/d"), me); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

// A root that sits under a system tree is not refused for it (a home in /var/home, a disk in /run/media, the temporary
// folder of a machine that keeps its home there), while the tree still applies outside every root, and the private
// folders of the home stay denied.
func TestFoldersSystemTreesApplyOutsideTheRoots(t *testing.T) {
	disk, _ := filepath.EvalSymlinks(t.TempDir())
	mk(t, filepath.Join(disk, "work"))
	e, home := folderEnv(t, func(o *Options) { o.BrowseRoots = func() []string { return []string{disk} } })
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	mk(t, filepath.Join(outside, "elsewhere"))
	privateName := hostpaths.DeniedTrees()[0]
	mk(t, filepath.Join(home, privateName, "x"), filepath.Join(home, "proj"), filepath.Join(home, "tree", "inner"))
	// the trees: the folder that holds the home folder, the one that holds the disk, and one inside the home
	e.s.opt.SystemTrees = []string{filepath.Dir(home), filepath.Dir(disk), outside, filepath.Join(home, "tree")}
	l, err := e.s.ListFolders("")
	if err != nil {
		t.Fatalf("a home inside a system tree: %v", err)
	}
	if strings.Join(names(l), ",") != "proj" { // "tree" is a system tree inside the root; the private folder is denied
		t.Errorf("home lists %v", names(l))
	}
	if d, err := e.s.ListFolders(disk); err != nil || strings.Join(names(d), ",") != "work" {
		t.Errorf("a disk inside a system tree: %v %v", names(d), err)
	}
	if _, err := e.s.refuseFolder(filepath.Join(home, "proj"), nil); err != nil {
		t.Errorf("a project in a home inside a system tree: %v", err)
	}
	if _, err := e.s.refuseFolder(filepath.Join(disk, "work"), nil); err != nil {
		t.Errorf("a project on a disk inside a system tree: %v", err)
	}
	// the tree applies outside the roots, and inside the root where the tree is the root's own child
	if _, err := e.s.refuseFolder(filepath.Join(outside, "elsewhere"), nil); err == nil || !strings.Contains(err.Error(), "system") {
		t.Errorf("a folder in a system tree outside every root: %v", err)
	}
	if _, err := e.s.ListFolders(filepath.Join(outside, "elsewhere")); err == nil {
		t.Error("a system tree outside every root was listed")
	}
	if _, err := e.s.ListFolders(filepath.Join(home, "tree")); err == nil {
		t.Error("a system tree inside the home was listed")
	}
	if _, err := e.s.refuseFolder(filepath.Join(home, "tree", "inner"), nil); err == nil {
		t.Error("a project inside a system tree inside the home")
	}
	// the private folders stay denied
	if _, err := e.s.ListFolders(filepath.Join(home, privateName)); err == nil {
		t.Error("a private folder was listed")
	}
	if _, err := e.s.refuseFolder(filepath.Join(home, privateName, "x"), nil); err == nil {
		t.Error("a private folder holds a project")
	}
}

func TestEntryActionTable(t *testing.T) {
	for name, c := range map[string]struct {
		mode os.FileMode
		want action
	}{
		"folder": {os.ModeDir, listEntry}, "file": {0, skipEntry}, "link": {os.ModeSymlink, followEntry},
		"junction (irregular)": {os.ModeIrregular, skipEntry}, "link that is also irregular": {os.ModeSymlink | os.ModeIrregular, skipEntry},
		"socket": {os.ModeSocket, skipEntry}, "pipe": {os.ModeNamedPipe, skipEntry}, "device": {os.ModeDevice, skipEntry},
		"folder that is irregular": {os.ModeDir | os.ModeIrregular, skipEntry},
	} {
		if got := entryAction(c.mode); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

// The decision after the folder was opened, on every host: the final path must be the folder asked for, inside the
// root, and not a denied place.
func TestVerifyFinalDecision(t *testing.T) {
	e, home := folderEnv(t)
	root := home
	asked := filepath.Join(home, "work")
	private := filepath.Join(home, hostpaths.DeniedTrees()[0])
	outside := filepath.Join(filepath.Dir(home), "elsewhere")
	for name, c := range map[string]struct {
		final string
		ok    bool
	}{
		"the folder itself":    {asked, true},
		"extended prefix":      {`\\?\` + asked, asked != "" && filepath.Separator == '\\'}, // only a Windows path has that prefix
		"another folder":       {filepath.Join(home, "other"), false},
		"outside the root":     {outside, false},
		"a denied place":       {private, false},
		"the parent":           {home, false},
		"a deeper folder":      {filepath.Join(asked, "inner"), false},
		"the same, other case": {strings.ToUpper(asked), true},
	} {
		err := e.s.verifyFinal(c.final, asked, home, root)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Unix: a folder that is swapped for a link, or for another folder, between the check and the use is caught after it
// was opened.
func TestVerifyOpenedCatchesASwap(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "work"), filepath.Join(home, "other"))
	asked := filepath.Join(home, "work")
	f, err := os.Open(asked)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := e.s.verifyOpened(f, asked, home, home); err != nil {
		t.Fatalf("the folder as it was: %v", err)
	}
	// the path now names another folder than the one that is open
	if err := os.Rename(asked, asked+".old"); err != nil {
		t.Fatal(err)
	}
	mk(t, asked)
	if err := e.s.verifyOpened(f, asked, home, home); err == nil {
		t.Error("another folder at the path went unnoticed")
	}
	// the path is a link now
	os.Remove(asked)
	if err := os.Symlink(filepath.Join(home, "other"), asked); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if err := e.s.verifyOpened(f, asked, home, home); err == nil {
		t.Error("a link at the path went unnoticed")
	}
}

func TestFoldersSayWhetherThisComputerRunsAgents(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "p"))
	l, _ := e.s.ListFolders("")
	if !l.Agents {
		t.Error("a Mac runs agents")
	}
	e.s.opt.GOOS = "linux"
	l, _ = e.s.ListFolders("")
	if l.Agents {
		t.Error("a computer that does not run agents says it does")
	}
	if _, err := e.s.AddProjectDir(filepath.Join(home, "p")); err == nil || !strings.Contains(err.Error(), "agents, which this computer doesn't run yet") {
		t.Errorf("project.add off a Mac: %v", err)
	}
}

// project.resolve answers the folder project.add would add, and adds nothing.
func TestProjectResolve(t *testing.T) {
	e, home := folderEnv(t)
	mk(t, filepath.Join(home, "real"))
	if err := os.Symlink(filepath.Join(home, "real"), filepath.Join(home, "link")); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	res, err := e.op("project.resolve", map[string]any{"dir": filepath.Join(home, "link")})
	if err != nil || res["dir"] != filepath.Join(home, "real") {
		t.Fatalf("%v %v", res, err)
	}
	if len(e.cfg.Projects) != 0 {
		t.Errorf("resolve added %v", e.cfg.Projects)
	}
	for _, bad := range []string{"", "relative", filepath.Join(home, "nope"), home} {
		if res, err := e.op("project.resolve", map[string]any{"dir": bad}); err == nil {
			t.Errorf("%q resolved to %v", bad, res)
		}
	}
	e.s.opt.GOOS = "linux"
	if _, err := e.op("project.resolve", map[string]any{"dir": filepath.Join(home, "real")}); err == nil {
		t.Error("resolve off a Mac")
	}
}

// A folder that is swapped for a link between the checks and the open is not listed: the opened folder is checked, not
// the path.
func TestFoldersASwapBetweenTheCheckAndTheOpenIsNotFollowed(t *testing.T) {
	e, home := folderEnv(t)
	private := filepath.Join(home, hostpaths.DeniedTrees()[0])
	mk(t, filepath.Join(home, "work", "fine"), filepath.Join(private, "secret-inside"))
	asked := filepath.Join(home, "work")
	e.s.beforeOpen = func(string) {
		if err := os.Rename(asked, asked+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(private, asked); err != nil {
			t.Skip("no symbolic links here:", err)
		}
	}
	l, err := e.s.ListFolders(asked)
	if err == nil {
		t.Fatalf("the swapped folder was listed: %+v", l)
	}
	if l != nil && len(l.Folders) != 0 {
		t.Errorf("entries of a private folder came back: %+v", l.Folders)
	}
}

// What the final path must be, when it is the folder asked for: still inside the root and not denied.
func TestVerifyFinalChecksTheRootAndTheDenyListsEvenForTheSamePath(t *testing.T) {
	e, home := folderEnv(t)
	private := filepath.Join(home, hostpaths.DeniedTrees()[0])
	outside := filepath.Join(filepath.Dir(home), "elsewhere")
	if err := e.s.verifyFinal(private, private, home, home); err == nil {
		t.Error("a denied place passed because it was also what was asked for")
	}
	if err := e.s.verifyFinal(outside, outside, home, home); err == nil {
		t.Error("a place outside the root passed because it was also what was asked for")
	}
	ok := filepath.Join(home, "work")
	if err := e.s.verifyFinal(ok, ok, home, home); err != nil {
		t.Errorf("a plain folder: %v", err)
	}
}
