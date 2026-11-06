//go:build unix

package microvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// sharePidFile is the pid file the launcher writes for share i's virtiofsd, in Spec.Dir (QEMU writes PidFile itself).
func sharePidFile(i int) string { return "fs" + strconv.Itoa(i) + ".pid" }

// ourFile is a name the launcher or the processes it starts put in a VM's folder.
var ourFile = regexp.MustCompile(`^(control\.sock|console\.sock|qemu\.pid|fs[0-9]{1,3}\.(sock|pid|sock\.pid)|[a-z0-9.]+\.tmp)$`)

// PrepareDir makes the VM's folder ready for a start: created with mode 0700 (only the user can enter it: it holds the
// sockets of the VM's channels), or, if it is there, checked to be a real folder of the user's own, set to 0700 and
// cleared of what an earlier run left. A symbolic link, another user's folder or a file there is refused. The folder's
// parent must exist (it is Home's own).
func PrepareDir(dir string) error {
	if err := hostPath(dir); err != nil {
		return fmt.Errorf("%w: the VM folder: %w", ErrSpec, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is not a folder", ErrSpec, dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s is not the user's own folder", ErrSpec, dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return clearDir(dir)
}

// clearDir removes what the launcher and its processes put in a VM's folder, and nothing else.
func clearDir(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if ourFile.MatchString(e.Name()) && !e.IsDir() {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// writePid writes a pid file whole (a reader never sees half of one).
func writePid(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readPid reads a pid file: a positive number, nothing else.
func readPid(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("%s: not a pid", path)
	}
	return pid, nil
}

// Sweeper ends what a Home that stopped without ending its VMs left: the QEMU and virtiofsd processes of the folders
// under Root, then the folders' files.
type Sweeper struct {
	// Root holds one folder per VM (Spec.Dir).
	Root string
	// Ours says whether pid is still a process this VM folder started (its command line names the folder), so a pid
	// reused by another program is never killed. Nil: the system's (Linux reads /proc/<pid>/cmdline).
	Ours func(pid int, dir string) bool
	// Kill ends a process (nil: SIGKILL).
	Kill func(pid int) error
}

// Sweep ends every VM process under Root and clears the folders; it carries on past a folder it can't clear and returns
// the errors together.
func (s Sweeper) Sweep() error {
	ours, kill := s.Ours, s.Kill
	if ours == nil {
		ours = processOfFolder
	}
	if kill == nil {
		kill = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
	}
	ents, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.Root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".pid") || !ourFile.MatchString(f.Name()) {
				continue
			}
			pid, err := readPid(filepath.Join(dir, f.Name()))
			if err != nil {
				continue // a half-written or foreign file: nothing to kill
			}
			if ours(pid, dir) {
				if err := kill(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
					errs = append(errs, fmt.Errorf("ending %d (%s): %w", pid, f.Name(), err))
				}
			}
		}
		if err := clearDir(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cmdlineNames says whether a process's command line (NUL-separated, as /proc gives it) names a file in dir.
func cmdlineNames(cmdline []byte, dir string) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for _, a := range strings.Split(string(cmdline), "\x00") {
		// QEMU's options carry paths inside option lists (path=<dir>/console.sock,server=on); a comma in dir is doubled
		if strings.Contains(a, prefix) || strings.Contains(a, optValue(prefix)) {
			return true
		}
	}
	return false
}
