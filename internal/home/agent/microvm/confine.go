package microvm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Access is what a Landlock rule lets a confined process do beneath a path.
type Access int

const (
	// AccessRead reads files and lists folders.
	AccessRead Access = iota + 1
	// AccessExec reads and runs programs (a program, the libraries it loads).
	AccessExec
	// AccessWrite reads, writes, makes and removes files, folders, links and sockets; never device files, and runs
	// nothing.
	AccessWrite
)

// FSRule lets a confined process reach Path (and what is beneath it, for a folder) with Access. Nothing else on the
// computer's file systems is reachable.
type FSRule struct {
	Path   string
	Access Access
}

// ErrNoLandlock is a computer whose kernel can't confine a process with Landlock (Linux before 5.13, or Landlock not
// enabled): the VM backend does not run there, since it would run an unconfined file server.
var ErrNoLandlock = errors.New("this computer's Linux kernel can't confine the file server (Landlock)")

// VirtiofsdRules is the Landlock rule set the virtiofsd of share i runs under: its shared folder (read-only for a
// read-only share), the VM's folder for its socket and lock file, its own program and libs (the folders of the libraries
// it loads: the pinned build's own folder), and /proc, which virtiofsd reopens its files through (/proc/self/fd). The
// rest of the computer is out of its reach, so a guest that takes over virtiofsd through a bug still sees only the
// share.
func VirtiofsdRules(bin string, spec Spec, i int, libs []string) ([]FSRule, error) {
	if err := hostPath(bin); err != nil {
		return nil, fmt.Errorf("%w: virtiofsd: %w", ErrSpec, err)
	}
	if err := spec.check(); err != nil {
		return nil, err
	}
	if i < 0 || i >= len(spec.Shares) {
		return nil, fmt.Errorf("%w: no shared folder %d", ErrSpec, i)
	}
	s := spec.Shares[i]
	share := AccessWrite
	if s.ReadOnly {
		share = AccessRead
	}
	rules := []FSRule{
		{Path: s.Source, Access: share},
		{Path: spec.Dir, Access: AccessWrite},
		{Path: bin, Access: AccessExec},
		{Path: "/proc", Access: AccessRead},
	}
	for _, l := range libs {
		if err := hostPath(l); err != nil {
			return nil, fmt.Errorf("%w: libraries: %w", ErrSpec, err)
		}
		if l == "/" {
			return nil, fmt.Errorf("%w: the whole computer as a library folder", ErrSpec)
		}
		rules = append(rules, FSRule{Path: l, Access: AccessExec})
	}
	return rules, nil
}

// checkRules refuses a rule set that would leave the confinement open: a rule for the root, or one for a path that does
// not exist (Landlock needs it opened).
func checkRules(rules []FSRule) error {
	for _, r := range rules {
		if err := hostPath(r.Path); err != nil {
			return err
		}
		if filepath.Clean(r.Path) == "/" {
			return errors.New("a Landlock rule for the whole computer")
		}
		if r.Access < AccessRead || r.Access > AccessWrite {
			return fmt.Errorf("%s: access %d", r.Path, r.Access)
		}
		if _, err := os.Stat(r.Path); err != nil {
			return err
		}
	}
	return nil
}
