package agentsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func runConfined(ctx context.Context, cmd *exec.Cmd, ownTree bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if ownTree {
		// PR_SET_PDEATHSIG follows the creating OS thread. Retain it until
		// namespace reaping, so thread retirement cannot kill a healthy task.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// A PID namespace must see its own procfs: srt/bwrap resolve /proc/PID/ns.
		// Use only a fixed system utility, never agent/ambient PATH.
		unshare, setpriv := "", ""
		for _, path := range []string{"/usr/bin/unshare", "/bin/unshare"} {
			if st, e := os.Stat(path); e == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 && st.Mode().Perm()&0022 == 0 {
				unshare = path
				break
			}
		}
		for _, path := range []string{"/usr/bin/setpriv", "/bin/setpriv"} {
			if st, e := os.Stat(path); e == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0 && st.Mode().Perm()&0022 == 0 {
				setpriv = path
				break
			}
		}
		if unshare == "" || setpriv == "" {
			return errors.New("sandbox: supervised tasks require the system util-linux unshare and setpriv utilities")
		}
		cmd.Args = append([]string{unshare, "--mount-proc=/proc", "--propagation", "private", "--", setpriv, "--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--", cmd.Path}, cmd.Args[1:]...)
		cmd.Path = unshare
		// Preserve the host UID inside the namespace: Claude refuses root.
		// Ambient mount/setpcap rights exist only in this new user namespace
		// and are removed by setpriv BEFORE executing any runtime/provider.
		// The trusted srt runtime is namespace PID 1. Linux does not finish reaping
		// it until every process in that namespace (including setsid/double-forks)
		// has exited. A process-group kill alone has no such completion guarantee.
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID, Pdeathsig: syscall.SIGKILL,
			UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}},
			GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false, AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SETPCAP}}
	}
	if err := cmd.Start(); err != nil {
		if ownTree {
			return fmt.Errorf("sandbox: supervised PID/user namespace unavailable (enable Linux unprivileged user namespaces): %w", err)
		}
		return err
	}
	// WNOWAIT retains our child's identity until process-group cleanup finishes.
	// bwrap --new-session is outside this group, but --die-with-parent tears down
	// its private PID namespace when either srt or the outer shell is killed.
	kill := func() {
		if ownTree {
			_ = cmd.Process.Kill()
			return
		}
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
	}
	done, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ctx.Done():
			kill()
		case <-done:
		}
	}()
	var info unix.Siginfo
	var waitErr error
	for {
		waitErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(waitErr, unix.EINTR) {
			break
		}
	}
	close(done)
	<-joined
	kill()
	// Killing the complete outer group also prevents pipe-copy goroutines hanging
	// on a provider's background shell. Reap only after the final group signal.
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Wait()
	if waitErr != nil {
		return waitErr
	}
	return err
}
