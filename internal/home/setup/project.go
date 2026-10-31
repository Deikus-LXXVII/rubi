package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// systemTrees are never projects, nor anything inside them; systemExact only themselves.
var (
	systemTrees = []string{"/System", "/Library", "/usr", "/bin", "/sbin", "/etc", "/var", "/private", "/dev", "/cores", "/Applications", "/tmp"}
	systemExact = []string{"/Volumes", "/Users", "/home", "/opt"}
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
	if real == string(filepath.Separator) {
		return "", errors.New("the whole disk can't be a project")
	}
	system := systemTrees
	if s.opt.SystemTrees != nil {
		system = s.opt.SystemTrees
	}
	for _, t := range system {
		if real == t || strings.HasPrefix(real, t+string(filepath.Separator)) {
			return "", errors.New("a system folder can't be a project")
		}
	}
	if slices.Contains(systemExact, real) {
		return "", errors.New("that folder holds other people's or other disks' folders; choose a folder inside it")
	}
	helper := s.opt.Dir
	if h, err := filepath.EvalSymlinks(helper); err == nil {
		helper = h
	}
	if helper != "" && (real == helper || strings.HasPrefix(real, helper+string(filepath.Separator))) {
		return "", errors.New("Rubi Home's own folder (it holds its keys) can't be a project")
	}
	if agent.ProtectedDir(real, s.opt.Dir) {
		return "", errors.New("your home folder, a private folder in it, a folder containing them, or Rubi Home's own folder can't be a project")
	}
	for _, e := range existing {
		if re, err := filepath.EvalSymlinks(filepath.Clean(e)); err == nil && re == real {
			return "", errors.New("that folder is already a project")
		}
	}
	if len(existing) >= MaxProjects {
		return "", errors.New("there are already 20 projects")
	}
	return real, nil
}

// AddProject shows the macOS folder chooser and adds the chosen folder to the projects. It returns the
// folder, or "" when the chooser was cancelled.
func (s *Setup) AddProject(ctx context.Context) (string, error) {
	if !s.mac() {
		return "", errNotMac
	}
	if len(s.opt.Settings().Projects) >= MaxProjects {
		return "", errors.New("there are already 20 projects")
	}
	s.mu.Lock()
	if s.choosing {
		s.mu.Unlock()
		return "", errBusy
	}
	s.choosing = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.choosing = false
		s.mu.Unlock()
	}()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := s.opt.Cmd.Run(cctx, []string{"/usr/bin/osascript", "-e",
		`POSIX path of (choose folder with prompt "Choose a folder the agents may work in")`})
	if err != nil {
		if cancelled(string(out)) {
			return "", nil
		}
		return "", errors.New("the folder chooser failed")
	}
	chosen := strings.TrimSpace(string(out))
	var real string
	var rerr error
	if uerr := s.opt.Update(func(c *Settings) {
		if real, rerr = s.refuseFolder(chosen, c.Projects); rerr == nil {
			c.Projects = append(c.Projects, real)
		}
	}); uerr != nil {
		return "", uerr
	}
	return real, rerr
}

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
	s.mu.Lock()
	delete(s.tests, dir)
	delete(s.loginSeen, dir)
	s.mu.Unlock()
	return nil
}
