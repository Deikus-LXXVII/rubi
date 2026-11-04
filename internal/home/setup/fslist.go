package setup

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// The folder browser of the Rubi panel (stage P3 of docs/plans/home-platforms.md). It replaces the native folder
// chooser of every system: the panel shows folders, the user taps one, and the panel sends the path to project.add,
// whose refuseFolder decides again.
//
// ListFolders is called by the panel through core (device.fs.list) and by nothing else: it is no capability of any
// plugin (core refuses "fs.list" from one). It is rate limited and counted by Home (fslimit.go), because a core that
// has been taken over holds the pairing token and could crawl it. It lists folders only, never a file, a size, a date or
// a byte of content, and only where a project could be:
//
//   - below the user's home folder, or below a removable disk of this user that is mounted (hostpaths.BrowseRoots),
//     never above them, and no deeper than maxFolderDepth levels below the root;
//   - never another user's home (a folder named Users or home on a disk lists only this user's own), nor the places
//     hostpaths.SystemExact and ExactOnlyDirs name;
//   - never a system tree outside the roots (a tree that holds a root does not apply inside it: a home in /var/home, a
//     disk in /run/media), a private folder of the home (hostpaths.SensitiveHomeDirs and DeniedTrees), or Rubi
//     Home's own folder. Such a folder is not listed, nor does it show up in its parent;
//   - never a folder whose name starts with a dot or has a control character in it;
//   - a link is listed only if it leads to a folder inside the same root that is not one of those; a Windows junction
//     (Go reports it as irregular) is not listed at all;
//   - the folder asked for is opened first and the opened folder is checked (its final path on Windows, os.SameFile on
//     Unix), and the entries are read from that open folder, so a link swapped in after the check leads nowhere.

// The bounds of one answer.
const (
	maxFolderEntries = 500
	maxFolderPath    = 4096
	maxFolderRead    = 20000
	maxFolderDepth   = 8 // levels below a root that are listed; the folder at this depth is listed without its sub-folders
)

// FolderEntry is one folder in a listing.
type FolderEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	CanChoose bool   `json:"can_choose"`
	Reason    string `json:"reason,omitempty"` // why it can not be chosen as a project
}

// FolderPlace is a place the browser can start in.
type FolderPlace struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// FolderList is the answer to ListFolders.
type FolderList struct {
	Path      string        `json:"path"`
	Name      string        `json:"name"`
	Parent    string        `json:"parent"` // "" at the top of a root
	CanChoose bool          `json:"can_choose"`
	Reason    string        `json:"reason,omitempty"`
	Agents    bool          `json:"agents"` // whether this computer runs agents, which projects are for
	Places    []FolderPlace `json:"places"`
	Folders   []FolderEntry `json:"folders"`
	Truncated bool          `json:"truncated,omitempty"` // there are more folders than are shown
	Deep      bool          `json:"deep,omitempty"`      // the folders below this one are not shown (too deep)
}

var (
	errNotBrowsable = errors.New("that place can't be shown in the folder browser")
	errChanged      = errors.New("that folder changed while it was opened; try again")
)

func plainName(n string) bool {
	return n != "" && !strings.HasPrefix(n, ".") && !strings.ContainsFunc(n, func(r rune) bool { return unicode.IsControl(r) || r == ' ' || r == ' ' })
}

// entryAction is what the listing does with a directory entry of the given type. Only a folder and a symbolic link are
// looked at; everything else is skipped: a file, a socket, a device, and a Windows junction or other reparse point,
// which Go reports as irregular.
type action int

const (
	skipEntry action = iota
	listEntry
	followEntry
)

func entryAction(typ os.FileMode) action {
	switch {
	case typ&os.ModeIrregular != 0:
		return skipEntry
	case typ&os.ModeSymlink != 0:
		return followEntry
	case typ.IsDir():
		return listEntry
	}
	return skipEntry
}

// currentUser is the name of the user, as a folder under Users or home is named: without a domain.
func currentUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	n := u.Username
	if i := strings.LastIndexAny(n, `\`); i >= 0 {
		n = n[i+1:]
	}
	return n
}

// userContainers are the folders that hold the users' homes.
var userContainers = []string{"users", "home"}

func isUserContainer(name string) bool {
	for _, c := range userContainers {
		if strings.EqualFold(name, c) {
			return true
		}
	}
	return false
}

// places returns the user's home folder (real path) and the roots the browser starts in, the home folder first.
func (s *Setup) places() (string, []FolderPlace, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", nil, errors.New("Rubi Home can't find the home folder")
	}
	if h, err := filepath.EvalSymlinks(home); err == nil {
		home = h
	}
	roots := []FolderPlace{{Name: "Home", Path: home}}
	list := hostpaths.BrowseRoots
	if s.opt.BrowseRoots != nil {
		list = s.opt.BrowseRoots
	}
	for _, r := range list() {
		real, err := filepath.EvalSymlinks(r)
		if err != nil {
			continue
		}
		if st, err := os.Stat(real); err != nil || !st.IsDir() || strings.EqualFold(real, home) {
			continue
		}
		name := filepath.Base(real)
		if filepath.Dir(real) == real { // a drive root has no base name worth showing
			name = real
		}
		roots = append(roots, FolderPlace{Name: name, Path: real})
	}
	return home, roots, nil
}

// rootOf is the root (home or a mounted disk) that holds real, else "".
func rootOf(real string, roots []FolderPlace) string {
	best := ""
	for _, r := range roots {
		if within(real, r.Path) && len(r.Path) > len(best) {
			best = r.Path
		}
	}
	return best
}

func within(p, root string) bool { return agent.Within(p, root) }

// relParts is the folder names from root down to p (p inside root).
func relParts(p, root string) []string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

// depthOf is how many levels p is below root.
func depthOf(p, root string) int { return len(relParts(p, root)) }

// otherUsersHome says whether p is in another user's home: below a Users or home folder, in a folder that is not the
// user's own (case does not matter).
func otherUsersHome(p, root, me string) bool {
	parts := relParts(p, root)
	for i := 0; i+1 < len(parts); i++ {
		if isUserContainer(parts[i]) && (me == "" || !strings.EqualFold(parts[i+1], me)) {
			return true
		}
	}
	return false
}

// systemHit says whether real is inside a system tree that applies to it: one that does not hold the root real is in
// (a user's home in /var/home, a disk in /run/media, or the temporary folder on a machine that keeps it there).
func (s *Setup) systemHit(real, root string) bool {
	system := hostpaths.SystemTrees()
	if s.opt.SystemTrees != nil {
		system = s.opt.SystemTrees
	}
	for _, t := range system {
		if within(real, t) && (root == "" || !within(root, t)) {
			return true
		}
	}
	return false
}

// browseDenied says whether a folder is one the browser never shows: a system tree that applies, a private folder of
// the home, Rubi Home's own folder, another user's home, or a place hostpaths lists as never a project itself.
func (s *Setup) browseDenied(real, home, root string) bool {
	if s.systemHit(real, root) {
		return true
	}
	for _, d := range append(hostpaths.SensitiveHomeDirs(), hostpaths.DeniedTrees()...) {
		if within(real, filepath.Join(home, d)) {
			return true
		}
	}
	if s.opt.Dir != "" {
		helper := s.opt.Dir
		if h, err := filepath.EvalSymlinks(helper); err == nil {
			helper = h
		}
		if within(real, helper) {
			return true
		}
	}
	if otherUsersHome(real, root, currentUser()) {
		return true
	}
	for _, e := range append(hostpaths.SystemExact(), hostpaths.ExactOnlyDirs()...) {
		if strings.EqualFold(real, e) {
			return true
		}
	}
	return false
}

// verifyOpened checks that the folder that was opened is the one that was checked: its final path (Windows) or the
// file itself (Unix) is still real, still inside the root, and still not a denied place.
func (s *Setup) verifyOpened(f *os.File, real, home, root string) error {
	final, err := fsx.FinalPath(f)
	if err == nil {
		return s.verifyFinal(final, real, home, root)
	}
	if !errors.Is(err, fsx.ErrNoFinalPath) {
		return errChanged
	}
	st, err := f.Stat()
	if err != nil {
		return errChanged
	}
	lst, err := os.Lstat(real)
	if err != nil || lst.Mode()&os.ModeSymlink != 0 || !os.SameFile(st, lst) {
		return errChanged
	}
	again, err := filepath.EvalSymlinks(real)
	if err != nil || !strings.EqualFold(again, real) {
		return errChanged
	}
	return nil
}

// verifyFinal is the Windows form of the check: the final path of the opened folder is the folder asked for and is
// not outside the root or in a denied place.
func (s *Setup) verifyFinal(final, real, home, root string) error {
	final = fsx.TrimExtendedPrefix(final)
	if !strings.EqualFold(filepath.Clean(final), filepath.Clean(real)) || !within(final, root) || s.browseDenied(final, home, root) {
		return errChanged
	}
	return nil
}

// ListFolders lists the folders in path ("" is the user's home folder).
func (s *Setup) ListFolders(path string) (*FolderList, error) {
	home, roots, err := s.places()
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = home
	}
	if len(path) > maxFolderPath || strings.ContainsFunc(path, unicode.IsControl) || !filepath.IsAbs(path) {
		return nil, errors.New("that isn't a folder path")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return nil, errors.New("that folder doesn't exist")
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return nil, errors.New("that isn't a folder")
	}
	root := rootOf(real, roots)
	depth := depthOf(real, root)
	if root == "" || depth > maxFolderDepth || s.browseDenied(real, home, root) {
		return nil, errNotBrowsable
	}
	existing := s.opt.Settings().Projects
	out := &FolderList{Path: real, Name: filepath.Base(real), Places: roots, Folders: []FolderEntry{}, Agents: s.mac()}
	if filepath.Dir(real) == real {
		out.Name = real
	}
	if !strings.EqualFold(real, root) {
		out.Parent = filepath.Dir(real)
	}
	if _, err := s.refuseFolder(real, existing); err != nil {
		out.Reason = err.Error()
	} else {
		out.CanChoose = true
	}

	// open it, check the opened folder, and read the entries from it: a link swapped in after the check is not followed
	if s.beforeOpen != nil {
		s.beforeOpen(real)
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, errors.New("that folder can't be read")
	}
	defer f.Close()
	if err := s.verifyOpened(f, real, home, root); err != nil {
		return nil, err
	}
	if depth >= maxFolderDepth {
		out.Deep = true
		return out, nil
	}
	ents, _ := f.ReadDir(maxFolderRead + 1) // one past the limit tells whether there is more; an unreadable one is empty
	readTruncated := len(ents) > maxFolderRead
	if readTruncated {
		ents = ents[:maxFolderRead]
	}
	var found []FolderEntry
	me := currentUser()
	container := isUserContainer(filepath.Base(real))
	for _, e := range ents {
		name := e.Name()
		if !plainName(name) {
			continue
		}
		if container && me != "" && !strings.EqualFold(name, me) { // another user's home is not shown
			continue
		}
		child := filepath.Join(real, name)
		switch entryAction(e.Type()) {
		case skipEntry:
			continue
		case followEntry:
			// a link: only if it leads to a folder inside this root
			target, err := filepath.EvalSymlinks(child)
			if err != nil {
				continue
			}
			if st, err := os.Stat(target); err != nil || !st.IsDir() || !within(target, root) {
				continue
			}
			child = target
		}
		if s.browseDenied(child, home, root) {
			continue
		}
		entry := FolderEntry{Name: name, Path: child}
		if _, err := s.refuseFolder(child, existing); err != nil {
			entry.Reason = err.Error()
		} else {
			entry.CanChoose = true
		}
		found = append(found, entry)
	}
	sort.Slice(found, func(i, j int) bool { return strings.ToLower(found[i].Name) < strings.ToLower(found[j].Name) })
	if len(found) > maxFolderEntries {
		found, out.Truncated = found[:maxFolderEntries], true
	}
	out.Truncated = out.Truncated || readTruncated
	out.Folders = append(out.Folders, found...)
	return out, nil
}
