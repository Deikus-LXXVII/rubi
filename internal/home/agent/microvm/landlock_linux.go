//go:build linux

package microvm

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// LandlockABI is the Landlock version of this computer's kernel (1 for Linux 5.13); ErrNoLandlock when it has none.
func LandlockABI() (int, error) {
	v, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		if e == unix.ENOSYS || e == unix.EOPNOTSUPP {
			return 0, ErrNoLandlock
		}
		return 0, fmt.Errorf("landlock: %w", e)
	}
	if int(v) < 1 {
		return 0, ErrNoLandlock
	}
	return int(v), nil
}

const fsFileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

// handled is every file system right the kernel's Landlock version knows: all of them are refused but what a rule allows.
func handled(abi int) uint64 {
	h := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		h |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		h |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		h |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return h
}

// rights is what an Access allows, before it is narrowed to the handled rights and, for a file, to a file's rights.
func rights(a Access) uint64 {
	read := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
	switch a {
	case AccessRead:
		return read
	case AccessExec:
		return read | unix.LANDLOCK_ACCESS_FS_EXECUTE
	case AccessWrite:
		return read | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER |
			unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return 0
}

// ExecConfined confines this process to rules with Landlock and then becomes argv[0] with env (as execve does): the
// program runs under the rule set and can't leave it, nor can anything it starts. It returns only on an error, and
// ErrNoLandlock when the kernel can't confine; nothing is run unconfined.
//
// The confinement is the calling thread's and is kept across execve, so the thread is locked from the rule set to the
// exec; no_new_privs (which Landlock needs unprivileged) also keeps set-user-id programs from gaining anything.
func ExecConfined(rules []FSRule, argv, env []string) error {
	if len(argv) == 0 {
		return errors.New("landlock: nothing to run")
	}
	if err := checkRules(rules); err != nil {
		return fmt.Errorf("landlock: %w", err)
	}
	runtime.LockOSThread() // never unlocked: the thread becomes the program or the process ends
	abi, err := LandlockABI()
	if err != nil {
		return err
	}
	h := handled(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: h}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return fmt.Errorf("landlock: create the rule set: %w", e)
	}
	ruleset := int(fd)
	defer unix.Close(ruleset)
	for _, r := range rules {
		pfd, err := unix.Open(r.Path, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("landlock: %s: %w", r.Path, err)
		}
		allowed := rights(r.Access) & h
		var st unix.Stat_t
		if err := unix.Fstat(pfd, &st); err != nil {
			unix.Close(pfd)
			return fmt.Errorf("landlock: %s: %w", r.Path, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			allowed &= fsFileRights
		}
		pb := unix.LandlockPathBeneathAttr{Allowed_access: allowed, Parent_fd: int32(pfd)}
		_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
			uintptr(unsafe.Pointer(&pb)), 0, 0, 0)
		unix.Close(pfd)
		if e != 0 {
			return fmt.Errorf("landlock: rule for %s: %w", r.Path, e)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("landlock: no_new_privs: %w", err)
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); e != 0 {
		return fmt.Errorf("landlock: restrict: %w", e)
	}
	err = unix.Exec(argv[0], argv, env)
	// confined but not run: end here rather than carry on as a confined Home
	fmt.Fprintf(os.Stderr, "landlock: exec %s: %v\n", argv[0], err)
	os.Exit(127)
	return nil
}
