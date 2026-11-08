package home

// Keeping Rubi Home running on a computer with no service manager to do it (docs/plans/t3-cloud-agents.md slice 2):
// a shared Linux box whose first process is a plain init, with no systemd user manager and no lingering. `rubi-home
// start` is safe to run any number of times: it starts one detached supervisor unless one runs already, and the
// supervisor runs `rubi-home run` and starts it again when it ends, waiting longer each time it fails quickly.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// SuperviseEnv is what the supervisor and `rubi-home start` work with.
type SuperviseEnv struct {
	Dir  string   // Rubi Home's folder: the lock and the log live there
	Exe  string   // the rubi-home binary
	Args []string // what the supervisor runs (default: run); what start runs is supervise
	Env  []string // the child's environment (os.Environ() when nil)
	// Backoff is the wait before the n-th quick restart in a row (superviseBackoff when nil).
	Backoff func(n int) time.Duration
	// Steady is how long a run must last for the next restart to count as the first again (superviseSteady when 0).
	Steady time.Duration
	// Grace is how long a stopping supervisor waits for the run to end before it kills it (superviseGrace when 0).
	Grace time.Duration
	Logf  func(format string, args ...any)
}

const (
	superviseSteady = time.Minute
	superviseGrace  = 30 * time.Second
	superviseLogMax = 10 << 20
)

// SupervisorLockPath is the file the one supervisor of a Rubi Home folder holds locked.
func SupervisorLockPath(dir string) string { return filepath.Join(dir, "supervisor.lock") }

// SupervisorLogPath is where the supervisor and the runs it starts write.
func SupervisorLogPath(dir string) string { return filepath.Join(dir, "rubi-home.log") }

// superviseBackoff waits 5 s before the first quick restart, doubling up to a minute.
func superviseBackoff(n int) time.Duration {
	d := 5 * time.Second
	for range min(n, 4) {
		d *= 2
	}
	return min(d, time.Minute)
}

func (e SuperviseEnv) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

// ErrSupervisorRunning says that another supervisor already holds the folder.
var ErrSupervisorRunning = errors.New("Rubi Home is already running")

// ErrAlreadyRunning says that another `rubi-home run` already serves the folder.
var ErrAlreadyRunning = errors.New("another rubi-home run already serves this folder")

// LockRun keeps a second `rubi-home run` off the folder: a run left by a supervisor that was killed would otherwise
// serve beside the new supervisor's. The lock lasts while the returned file is open.
func LockRun(dir string) (*os.File, error) {
	return lockExclusive(filepath.Join(dir, "run.lock"), ErrAlreadyRunning)
}

// lockSupervisor takes the folder's supervisor lock without waiting.
func lockSupervisor(dir string) (*os.File, error) {
	return lockExclusive(SupervisorLockPath(dir), ErrSupervisorRunning)
}

// lockExclusive locks path without waiting (busy when someone holds it) and writes this process's id into it.
func lockExclusive(path string, busy error) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := fsx.OpenNoFollow(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := fsx.TryLock(f); err != nil {
		f.Close()
		if errors.Is(err, fsx.ErrLocked) {
			return nil, busy
		}
		return nil, err
	}
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return f, nil
}

// SupervisorRunning reports whether a supervisor holds the folder now.
func SupervisorRunning(dir string) bool {
	f, err := fsx.OpenNoFollow(SupervisorLockPath(dir), os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := fsx.TryLock(f); err != nil {
		return errors.Is(err, fsx.ErrLocked)
	}
	fsx.Unlock(f)
	return false
}

// openSupervisorLog opens the log for appending, first moving a log over the limit to rubi-home.log.1.
func openSupervisorLog(dir string) (*os.File, error) {
	p := SupervisorLogPath(dir)
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > superviseLogMax {
		_ = os.Rename(p, p+".1")
	}
	f, err := fsx.OpenNoFollow(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Chmod(0o600)
	return f, nil
}

// Supervise holds the folder's supervisor lock and keeps Args running until ctx ends, then stops the run (a polite
// signal, a kill after Grace) and returns. It returns ErrSupervisorRunning at once when another supervisor holds it.
func Supervise(ctx context.Context, e SuperviseEnv) error {
	lock, err := lockSupervisor(e.Dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	args := e.Args
	if len(args) == 0 {
		args = []string{"run"}
	}
	backoff, steady, grace := e.Backoff, e.Steady, e.Grace
	if backoff == nil {
		backoff = superviseBackoff
	}
	if steady <= 0 {
		steady = superviseSteady
	}
	if grace <= 0 {
		grace = superviseGrace
	}
	quick := 0
	for {
		began := time.Now()
		err := superviseOnce(ctx, e, args, grace)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(began) >= steady {
			quick = 0
		}
		wait := backoff(quick)
		quick++
		e.logf("rubi-home supervisor: the run ended (%v); starting it again in %s", err, wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// superviseOnce runs args once, with the log as its output, until it ends or ctx does.
func superviseOnce(ctx context.Context, e SuperviseEnv, args []string, grace time.Duration) error {
	lf, err := openSupervisorLog(e.Dir)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(e.Exe, args...)
	cmd.Env = e.Env
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	_ = terminate(cmd.Process)
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
	}
	_ = cmd.Process.Kill()
	return <-done
}

// StartSupervisor is `rubi-home start`: it starts a detached supervisor unless one runs, and waits until that one
// holds the lock. already is true when one was running.
func StartSupervisor(e SuperviseEnv) (already bool, err error) {
	if SupervisorRunning(e.Dir) {
		return true, nil
	}
	if err := os.MkdirAll(e.Dir, 0o700); err != nil {
		return false, err
	}
	lf, err := openSupervisorLog(e.Dir)
	if err != nil {
		return false, err
	}
	defer lf.Close()
	args := e.Args
	if len(args) == 0 {
		args = []string{"supervise"}
	}
	cmd := exec.Command(e.Exe, args...)
	cmd.Env = e.Env
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Dir = "/"
	if err := detach(cmd); err != nil {
		return false, err
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.After(10 * time.Second)
	for {
		if SupervisorRunning(e.Dir) {
			return false, nil // the goroutine above stays waiting on it; start is a short command that ends now
		}
		select {
		case <-exited:
			// It lost the race to another start (which now runs) or failed: the log says which.
			if SupervisorRunning(e.Dir) {
				return true, nil
			}
			return false, fmt.Errorf("the supervisor ended at once; see %s", SupervisorLogPath(e.Dir))
		case <-deadline:
			return false, fmt.Errorf("the supervisor didn't come up; see %s", SupervisorLogPath(e.Dir))
		case <-time.After(50 * time.Millisecond):
		}
	}
}
