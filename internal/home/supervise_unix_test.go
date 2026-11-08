//go:build unix

package home

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestSuperviseHelper plays the supervised run (and, for start, the supervisor) when the tests run the test binary
// with RUBI_SUPERVISE_HELPER set. It notes each start in "starts" under HELPER_DIR.
func TestSuperviseHelper(t *testing.T) {
	mode := os.Getenv("RUBI_SUPERVISE_HELPER")
	if mode == "" {
		t.Skip("helper for the supervisor tests")
	}
	dir := os.Getenv("HELPER_DIR")
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM) // before the start is noted, so a test's stop never meets the default action
	f, _ := os.OpenFile(filepath.Join(dir, "starts"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
	f.Close()
	switch mode {
	case "exit":
		os.Exit(1)
	case "stay", "stubborn":
		if mode == "stubborn" {
			for {
				<-c
			}
		}
		select {
		case <-c:
			_ = os.WriteFile(filepath.Join(dir, "term"), nil, 0o600)
			os.Exit(0)
		case <-time.After(time.Minute):
			os.Exit(0)
		}
	case "hold": // a supervisor: holds the lock
		l, err := lockSupervisor(dir)
		if err != nil {
			os.Exit(3)
		}
		defer l.Close()
		time.Sleep(time.Minute)
		os.Exit(0)
	case "fail":
		os.Exit(4)
	}
}

func helperEnv(t *testing.T, mode, dir string) SuperviseEnv {
	return SuperviseEnv{
		Dir: dir, Exe: os.Args[0], Args: []string{"-test.run=^TestSuperviseHelper$"},
		Env:     append(os.Environ(), "RUBI_SUPERVISE_HELPER="+mode, "HELPER_DIR="+dir),
		Backoff: func(int) time.Duration { return 10 * time.Millisecond },
		Grace:   2 * time.Second,
	}
}

func starts(dir string) []int {
	b, _ := os.ReadFile(filepath.Join(dir, "starts"))
	var out []int
	for _, l := range strings.Fields(string(b)) {
		n, _ := strconv.Atoi(l)
		out = append(out, n)
	}
	return out
}

func waitSup(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// supervise runs Supervise in the background; the returned func stops it and returns its error.
func supervise(t *testing.T, e SuperviseEnv) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- Supervise(ctx, e) }()
	var once sync.Once
	var err error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-res:
			case <-time.After(20 * time.Second):
				err = errors.New("Supervise didn't return")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func TestSuperviseRestartsTheRun(t *testing.T) {
	dir := t.TempDir()
	e := helperEnv(t, "exit", dir)
	var mu sync.Mutex
	var waits []int
	e.Backoff = func(n int) time.Duration {
		mu.Lock()
		waits = append(waits, n)
		mu.Unlock()
		return 10 * time.Millisecond
	}
	e.Steady = time.Hour
	stop := supervise(t, e)
	waitSup(t, "three runs", func() bool { return len(starts(dir)) >= 3 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(waits) < 2 || waits[0] != 0 || waits[1] != 1 {
		t.Errorf("quick restarts waited %v; want the wait to grow with each", waits)
	}
}

func TestSuperviseSteadyRunResetsTheWait(t *testing.T) {
	dir := t.TempDir()
	e := helperEnv(t, "exit", dir)
	var mu sync.Mutex
	var waits []int
	e.Backoff = func(n int) time.Duration {
		mu.Lock()
		waits = append(waits, n)
		mu.Unlock()
		return 10 * time.Millisecond
	}
	e.Steady = time.Nanosecond // every run counts as steady
	stop := supervise(t, e)
	waitSup(t, "three runs", func() bool { return len(starts(dir)) >= 3 })
	_ = stop()
	mu.Lock()
	defer mu.Unlock()
	for _, n := range waits {
		if n != 0 {
			t.Fatalf("waits %v after steady runs; want each to be the first", waits)
		}
	}
}

func TestSuperviseStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	stop := supervise(t, helperEnv(t, "stay", dir))
	waitSup(t, "the run", func() bool { return len(starts(dir)) == 1 })
	if !SupervisorRunning(dir) {
		t.Error("the supervisor's lock isn't held")
	}
	second, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a supervisor that wrongly got the lock ends
	defer cancel()
	if err := Supervise(second, helperEnv(t, "stay", dir)); !errors.Is(err, ErrSupervisorRunning) {
		t.Errorf("a second supervisor: %v", err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "term")); err != nil {
		t.Error("the run wasn't asked to stop")
	}
	if SupervisorRunning(dir) {
		t.Error("the lock is still held after the supervisor ended")
	}
	if len(starts(dir)) != 1 {
		t.Error("the run was started again while stopping")
	}
}

func TestSuperviseKillsARunThatWontStop(t *testing.T) {
	dir := t.TempDir()
	e := helperEnv(t, "stubborn", dir)
	e.Grace = 200 * time.Millisecond
	stop := supervise(t, e)
	waitSup(t, "the run", func() bool { return len(starts(dir)) == 1 })
	began := time.Now()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if time.Since(began) > 10*time.Second {
		t.Error("stopping waited far past the grace")
	}
	pid := starts(dir)[0]
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the run that ignored the signal is still running")
	}
}

func TestStartSupervisor(t *testing.T) {
	dir := t.TempDir()
	e := helperEnv(t, "hold", dir)
	res, err := StartSupervisor(e)
	if err != nil || res != StartedNow {
		t.Fatalf("start = %v, %v", res, err)
	}
	t.Cleanup(func() {
		if s := starts(dir); len(s) > 0 {
			_ = syscall.Kill(s[0], syscall.SIGKILL)
		}
	})
	if !SupervisorRunning(dir) {
		t.Fatal("no supervisor after start")
	}
	pid := starts(dir)[0]
	if sid, _ := unix.Getsid(pid); sid != pid {
		t.Errorf("the supervisor's session is %d; want its own (%d)", sid, pid)
	}
	e.SystemStarts = func(context.Context) bool { t.Error("the system was asked although a supervisor runs"); return true }
	if res, err := StartSupervisor(e); err != nil || res != AlreadyRunning {
		t.Errorf("a second start = %v, %v; want already running", res, err)
	}
	if len(starts(dir)) != 1 {
		t.Error("a second start started another supervisor")
	}
	if fi, err := os.Stat(SupervisorLogPath(dir)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log: %v %v", fi, err)
	}
}

func TestStartSupervisorThatFails(t *testing.T) {
	dir := t.TempDir()
	if _, err := StartSupervisor(helperEnv(t, "fail", dir)); err == nil || !strings.Contains(err.Error(), SupervisorLogPath(dir)) {
		t.Errorf("start of a failing supervisor: %v", err)
	}
}

func TestLockRun(t *testing.T) {
	dir := t.TempDir()
	l, err := LockRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockRun(dir); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("a second run: %v", err)
	}
	l.Close()
	l2, err := LockRun(dir)
	if err != nil {
		t.Errorf("after the first run ended: %v", err)
	} else {
		l2.Close()
	}
}

func TestSupervisorLogIsRotated(t *testing.T) {
	dir := t.TempDir()
	p := SupervisorLogPath(dir)
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, superviseLogMax+1); err != nil {
		t.Fatal(err)
	}
	f, err := openSupervisorLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, err := os.Stat(p); err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("new log: %v %v", fi, err)
	}
	if fi, err := os.Stat(p + ".1"); err != nil || fi.Size() != superviseLogMax+1 {
		t.Errorf("rotated log: %v %v", fi, err)
	}
}

func TestSuperviseBackoff(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for n, w := range want {
		if got := superviseBackoff(n); got != w {
			t.Errorf("superviseBackoff(%d) = %s, want %s", n, got, w)
		}
	}
}

func TestSupervisorLogIsMadePrivate(t *testing.T) {
	dir := t.TempDir()
	p := SupervisorLogPath(dir)
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openSupervisorLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("an earlier readable log stays readable: %v %v", fi, err)
	}
}

func TestSupervisorLockOutlastsAProbe(t *testing.T) {
	dir := t.TempDir()
	probe, err := lockExclusive(SupervisorLockPath(dir), ErrSupervisorRunning) // what SupervisorRunning holds for a moment
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(100 * time.Millisecond); probe.Close() }()
	l, err := lockSupervisor(dir)
	if err != nil {
		t.Fatalf("a supervisor gave up because of a probe: %v", err)
	}
	l.Close()
}

func TestStartSupervisorLeavesItToTheSystem(t *testing.T) {
	dir := t.TempDir()
	e := helperEnv(t, "hold", dir)
	e.SystemStarts = func(context.Context) bool { return true }
	if res, err := StartSupervisor(e); err != nil || res != StartedBySystem {
		t.Fatalf("start where the system starts Rubi Home = %v, %v", res, err)
	}
	if len(starts(dir)) != 0 || SupervisorRunning(dir) {
		t.Error("a supervisor was started beside the system's start")
	}
}
