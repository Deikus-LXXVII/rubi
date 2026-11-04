package microvm

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// GuestUID is the user the runner runs as inside the VM (the runner image's "rubi", agent.runnerUID); its files are the
// user's own on the computer.
const GuestUID = 10001

// HostIDs are the user and group virtiofsd runs as: the person's own, never root.
type HostIDs struct {
	UID, GID int
}

// VirtiofsdArgs is the command line of the virtiofsd that serves share i of spec. It runs as the user, never root.
//
// It runs with --sandbox none: virtiofsd's own sandbox (namespace) needs a user namespace with capabilities, which Ubuntu
// 24.04 refuses to unconfined programs (kernel.apparmor_restrict_unprivileged_userns), and Rubi Home installs nothing as
// root to change that. The confinement to the shared folder is instead the launcher's: it applies a Landlock rule set
// that lets the process reach only that folder (read-only for a read-only share) before it starts virtiofsd
// (docs/design/linux-microvm.md). virtiofsd's own seccomp filter stays on (its default).
//
// The guest's runner (GuestUID) and the user are one and the same: --translate-uid and --translate-gid map the one to the
// other, so what the agent writes is the user's and nothing it writes can be another user's. A read-only share is also
// refused writes by virtiofsd itself (--readonly). Extended attributes stay off (virtiofsd's default), and the cache
// follows the host's changes (--cache auto), since the user's editor changes the project while the agent works.
func VirtiofsdArgs(bin string, spec Spec, i int, ids HostIDs) ([]string, error) {
	if err := hostPath(bin); err != nil {
		return nil, fmt.Errorf("%w: virtiofsd: %w", ErrSpec, err)
	}
	if err := spec.check(); err != nil {
		return nil, err
	}
	if i < 0 || i >= len(spec.Shares) {
		return nil, fmt.Errorf("%w: no shared folder %d", ErrSpec, i)
	}
	if ids.UID <= 0 || ids.GID <= 0 {
		return nil, fmt.Errorf("%w: virtiofsd must not run as root", ErrSpec)
	}
	s := spec.Shares[i]
	args := []string{bin,
		"--socket-path", filepath.Join(spec.Dir, ShareSocket(i)),
		"--shared-dir", s.Source,
		"--sandbox", "none",
		"--translate-uid", "map:" + strconv.Itoa(GuestUID) + ":" + strconv.Itoa(ids.UID) + ":1",
		"--translate-gid", "map:" + strconv.Itoa(GuestUID) + ":" + strconv.Itoa(ids.GID) + ":1",
		"--cache", "auto",
	}
	if s.ReadOnly {
		args = append(args, "--readonly")
	}
	return args, nil
}
