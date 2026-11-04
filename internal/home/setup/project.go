package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// systemTrees are never projects, nor anything inside them; systemExact only themselves.
var (
	systemTrees = hostpaths.SystemTrees()
	systemExact = hostpaths.SystemExact()
)

// refuseFolder says why dir (an absolute path) can't be a project, or returns its real path.
func (s *Setup) refuseFolder(dir string, existing []string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("that isn't a folder path")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", errors.New("that folder doesn't exist")
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", errors.New("that isn't a folder")
	}
	if filepath.Dir(real) == real { // "/" or a drive root
		return "", errors.New("the whole disk can't be a project")
	}
	home, roots, _ := s.places()
	root := rootOf(real, roots)
	if s.systemHit(real, root) {
		return "", errors.New("a system folder can't be a project")
	}
	for _, r := range roots[min(1, len(roots)):] { // a disk's own root: a project is a folder on it
		if strings.EqualFold(real, r.Path) {
			return "", errors.New("the root of a disk can't be a project; choose a folder inside it")
		}
	}
	if slices.ContainsFunc(systemExact, func(e string) bool { return strings.EqualFold(e, real) }) {
		return "", errors.New("that folder holds other people's or other disks' folders; choose a folder inside it")
	}
	if reason := sharedPlace(real, home); reason != "" {
		return "", errors.New(reason)
	}
	helper := s.opt.Dir
	if h, err := filepath.EvalSymlinks(helper); err == nil {
		helper = h
	}
	if helper != "" && agent.Within(real, helper) {
		return "", errors.New("Rubi Home's own folder (it holds its keys) can't be a project")
	}
	if agent.ProtectedDir(real, s.opt.Dir) {
		return "", errors.New("your home folder, a private folder in it, a folder containing them, or Rubi Home's own folder can't be a project")
	}
	for _, e := range existing {
		if re, err := filepath.EvalSymlinks(filepath.Clean(e)); err == nil && strings.EqualFold(re, real) {
			return "", errors.New("that folder is already a project")
		}
	}
	if len(existing) >= MaxProjects {
		return "", errors.New("there are already 20 projects")
	}
	return real, nil
}

// AddProjectDir adds dir (an absolute path, the folder a Bot named and the user approved) to the projects, with
// the same checks as a chosen folder. It returns the folder's real path.
func (s *Setup) AddProjectDir(dir string) (string, error) {
	if !s.mac() {
		return "", errNoAgentsHere
	}
	var real string
	var rerr error
	if uerr := s.opt.Update(func(c *Settings) {
		if real, rerr = s.refuseFolder(dir, c.Projects); rerr == nil {
			c.Projects = append(c.Projects, real)
		}
	}); uerr != nil {
		return "", uerr
	}
	return real, rerr
}

// errNoAgentsHere answers a project.add on a computer that does not run agents (projects are for agents; they run on
// a Mac for now). The folder browser says it before the user picks (FolderList.Agents).
var errNoAgentsHere = errors.New("projects are for agents, which this computer doesn't run yet")

// ResolveProjectDir is what project.add would add for dir: its real path, after the same checks, without adding
// anything. Core asks it before it lets a project.add through, so that the folder the user approved is the folder Home
// adds, and a plugin can show the user the path as Home resolves it.
func (s *Setup) ResolveProjectDir(dir string) (string, error) {
	if !s.mac() {
		return "", errNoAgentsHere
	}
	return s.refuseFolder(dir, s.opt.Settings().Projects)
}

// errChooseInPanel answers a project.add that names no folder. Home shows no folder chooser of its own on any system:
// the folder is chosen in the Rubi panel's folder browser (fs.list), and project.add gets its path.
var errChooseInPanel = errors.New("choose the folder in the Rubi panel; Rubi Home shows no folder chooser")

// RemoveProject removes a project folder from the configuration (the folder itself is not touched).
func (s *Setup) RemoveProject(dir string) error {
	s.mu.Lock()
	busy := s.job != nil && s.job.project.Dir != "" && s.job.project.Dir == dir
	s.mu.Unlock()
	if busy {
		return errBusy
	}
	found := false
	err := s.opt.Update(func(c *Settings) {
		var kept []string
		for _, e := range c.Projects {
			re, err := filepath.EvalSymlinks(filepath.Clean(e))
			if e == dir || err == nil && re == dir {
				found = true
				continue
			}
			kept = append(kept, e)
		}
		if found {
			c.Projects = kept
		}
	})
	if err != nil {
		return err
	}
	if !found {
		return errors.New("that folder isn't a project")
	}
	return nil
}

// cutPrefixFold is strings.CutPrefix ignoring case.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// sharedPlace says why a path is no project because it belongs to someone or something else: the root of a
// volume (/Volumes/<name>), /Users/Shared, or another user's home folder or anything in it.
func sharedPlace(real, home string) string {
	sep := string(filepath.Separator)
	if rest, ok := cutPrefixFold(real, "/Volumes"+sep); ok && !strings.Contains(rest, sep) {
		return "the root of a disk can't be a project; choose a folder inside it"
	}
	if rest, ok := cutPrefixFold(real, "/Users"+sep); ok {
		name, _, _ := strings.Cut(rest, sep)
		if !strings.EqualFold("/Users"+sep+name, home) {
			return "another user's folder (or the shared folder) can't be a project"
		}
	}
	return ""
}
