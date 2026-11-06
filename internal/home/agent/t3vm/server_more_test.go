package t3vm

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// ---- the sweep and the lock ----

func TestServeSweepsOldVMsAndEnvFilesAndKeepsOtherContainers(t *testing.T) {
	var envDir string
	w := newWorldWith(t, func(w *world) {
		w.host.list = []string{"buildkit", "rubi-agent-my-project-ab12", "rubi-t3-r-aaaaaa01-0011223344556677", "rubi-t3-ver-8899aabbccddeeff"}
		envDir = filepath.Join(w.dir, "t3vm", "env")
		os.MkdirAll(filepath.Join(envDir, "sub"), 0o700)
		os.WriteFile(filepath.Join(envDir, "old.env"), []byte("HTTPS_PROXY=http://rubi:secret@x\n"), 0o600)
		os.WriteFile(filepath.Join(envDir, "sub", "x"), []byte("x"), 0o600)
	})
	_ = w
	eventually(t, "the sweep", func() bool { _, rm, _ := w.host.snapshot(); return len(rm) == 2 })
	_, rm, _ := w.host.snapshot()
	if rm[0] != "rubi-t3-r-aaaaaa01-0011223344556677" || rm[1] != "rubi-t3-ver-8899aabbccddeeff" {
		t.Fatalf("removed %q: only rubi-t3-* containers go", rm)
	}
	if ents, _ := os.ReadDir(envDir); len(ents) != 0 {
		t.Errorf("env files left: %v", ents)
	}
	if fi, err := os.Stat(envDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("env dir: %v %v", fi, err)
	}
}

func TestServeSweepFailureIsLoggedNotFatal(t *testing.T) {
	w := newWorldWith(t, func(w *world) { w.host.listErr = errors.New("tool down") })
	if _, rep := w.start(nil); !rep.OK {
		t.Fatalf("%+v", rep)
	}
}

func TestSecondServerOnTheSameDirectoryIsRefusedAndKeepsTheLiveVMs(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	w.host.mu.Lock()
	w.host.list = []string{rep.Container} // the live VM is in the tool's list, as it would be
	w.host.mu.Unlock()
	second := NewServer(w.srv.opt)
	sd, _ := os.MkdirTemp("", "s")
	defer os.RemoveAll(sd)
	err := second.Serve(context.Background(), filepath.Join(sd, "s2.sock"))
	if err == nil || !strings.Contains(err.Error(), "another Rubi Home") {
		t.Fatalf("second Serve: %v", err)
	}
	if _, rm, _ := w.host.snapshot(); len(rm) != 0 {
		t.Fatalf("the second server removed %q", rm)
	}
	if _, err := os.Stat(w.sock); err != nil {
		t.Errorf("the first server's socket is gone: %v", err)
	}
	// and when the first one is done the lock is free again
	if err := w.stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(w.srv.opt).Serve(ctx, filepath.Join(sd, "s3.sock")) }()
	eventually(t, "the third server to listen", func() bool { _, err := os.Stat(filepath.Join(sd, "s3.sock")); return err == nil })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// ---- the caps ----

func TestAPlaceUnderTheCapIsFreedAsSoonAsTheConnectionIsGone(t *testing.T) {
	w := newWorld(t)
	w.be.perRole, w.be.total = 1, 10
	w.host.removeGate = make(chan struct{}) // removing the VM takes "forever"
	defer func() {
		select {
		case <-w.host.removeGate:
		default:
			close(w.host.removeGate)
		}
	}()
	c, rep := w.start(nil)
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	c.Close()
	// at once: the first VM is still being removed, the place is free
	c2, rep2 := w.start(nil)
	if !rep2.OK {
		t.Fatalf("a start right after a stop was refused: %+v", rep2)
	}
	c2.Close()
}

func TestAStartWaitsForAPlaceThatIsAboutToBeFree(t *testing.T) {
	old := admitRetry
	admitRetry = 5 * time.Second
	defer func() { admitRetry = old }()
	w := newWorld(t)
	admitRetry = 5 * time.Second
	w.be.perRole, w.be.total = 1, 10
	c, rep := w.start(nil)
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	got := make(chan StartReply, 1)
	go func() {
		c2, r := w.start(nil)
		defer c2.Close()
		got <- r
	}()
	time.Sleep(300 * time.Millisecond) // the second start is waiting
	select {
	case r := <-got:
		t.Fatalf("answered while the cap was full: %+v", r)
	default:
	}
	c.Close()
	select {
	case r := <-got:
		if !r.OK {
			t.Fatalf("the waiting start was refused after the place was freed: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting start never got the place")
	}
}

func TestAStartOverTheCapIsRefusedAfterTheWait(t *testing.T) {
	w := newWorld(t)
	w.be.perRole = 1
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	start := time.Now()
	_, rep2 := w.start(nil)
	if rep2.OK || !strings.Contains(rep2.Error, "this role already runs 1") {
		t.Fatalf("%+v", rep2)
	}
	if d := time.Since(start); d < admitRetry-50*time.Millisecond {
		t.Errorf("refused after %s, before the wait of %s was over", d, admitRetry)
	}
}

// ---- the caller ----

func TestDescendsFrom(t *testing.T) {
	parents := map[int]int{500: 400, 400: 300, 300: 200, 200: 1}
	lookup := func(p int) (int, error) {
		if pp, ok := parents[p]; ok {
			return pp, nil
		}
		return 0, errors.New("gone")
	}
	for _, c := range []struct {
		pid, anc int
		want     bool
	}{{500, 300, true}, {500, 400, true}, {500, 200, true}, {500, 500, false}, {300, 500, false}, {500, 999, false}, {777, 300, false}} {
		if got := descendsFrom(c.pid, c.anc, lookup); got != c.want {
			t.Errorf("descendsFrom(%d, %d) = %v", c.pid, c.anc, got)
		}
	}
	// a long chain is not followed for ever
	long := func(p int) (int, error) { return p + 1, nil }
	if descendsFrom(2, 1000, long) {
		t.Error("the walk up the tree has no limit")
	}
}

func TestServerWithAManagedT3OnlyServesItsDescendants(t *testing.T) {
	// this process is the shim: its parent (the go test runner) stands for T3, a stranger does not
	parent := os.Getppid()
	ok := newWorld(t, func(o *Options) { o.T3Pid = func() int { return parent } })
	if c, rep := ok.start(nil); !rep.OK {
		t.Fatalf("a descendant of T3 was refused: %+v", rep)
	} else {
		c.Close()
	}
	stranger := exec.Command("sleep", "30")
	if err := stranger.Start(); err != nil {
		t.Skip("no sleep")
	}
	defer func() { stranger.Process.Kill(); stranger.Wait() }()
	bad := newWorld(t, func(o *Options) { o.T3Pid = func() int { return stranger.Process.Pid } })
	c, rep := bad.start(nil)
	defer c.Close()
	if rep.OK || !strings.Contains(rep.Error, "own user") {
		t.Fatalf("a process outside T3 was served: %+v", rep)
	}
	if runs, _, _ := bad.host.snapshot(); len(runs) != 0 {
		t.Fatal("a VM was started")
	}
	// T3 not running (pid 0) or not managed by Home: the user id is what counts
	for _, pid := range []func() int{func() int { return 0 }, nil} {
		w := newWorld(t, func(o *Options) { o.T3Pid = pid })
		if c, rep := w.start(nil); !rep.OK {
			t.Fatalf("fallback refused: %+v", rep)
		} else {
			c.Close()
		}
	}
}

// ---- version probe ----

func TestVersionProbeIsOneAtATimeNamedAndForgottenWithItsConnection(t *testing.T) {
	w := newWorld(t)
	w.host.outputGate = make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan StartReply, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, rep := w.session(StartRequest{Op: OpVersion, Role: testRole, Argv: []string{"--version"}})
			c.Close()
			results <- rep
		}()
	}
	eventually(t, "a probe to be running", func() bool { w.host.mu.Lock(); defer w.host.mu.Unlock(); return len(w.host.outputs) == 1 })
	time.Sleep(200 * time.Millisecond) // the others have joined it
	close(w.host.outputGate)
	wg.Wait()
	close(results)
	for r := range results {
		if !r.OK || r.Output != "2.1.314 (Claude Code)\n" {
			t.Errorf("%+v", r)
		}
	}
	w.host.mu.Lock()
	n, argv := len(w.host.outputs), strings.Join(w.host.outputs[0], " ")
	w.host.mu.Unlock()
	if n != 1 {
		t.Errorf("%d probes ran for three callers", n)
	}
	if !strings.Contains(argv, "--name rubi-t3-ver-") || !strings.HasPrefix(argv, "/nonexistent-tools/container run --rm") {
		t.Errorf("probe command: %s", argv)
	}

	// a caller whose connection is gone is not waited for
	w2 := newWorld(t)
	w2.host.outputGate = make(chan struct{})
	defer close(w2.host.outputGate)
	c, err := net.Dial("unix", w2.sock)
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte(`{"op":"claude.version","role":"` + testRole + `","argv":["--version"]}` + "\n"))
	eventually(t, "the probe to start", func() bool { w2.host.mu.Lock(); defer w2.host.mu.Unlock(); return len(w2.host.outputs) == 1 })
	c.Close()
	eventually(t, "the connection to be dropped", func() bool {
		w2.srv.mu.Lock()
		defer w2.srv.mu.Unlock()
		return w2.srv.pending == 0
	})
}

func TestVersionProbeFailureIsRememberedForAWhile(t *testing.T) {
	old := versionRetry
	defer func() { versionRetry = old }()
	versionRetry = 400 * time.Millisecond
	w := newWorld(t)
	w.host.outputErr = errors.New("image broken")
	probe := func() StartReply {
		c, rep := w.session(StartRequest{Op: OpVersion, Role: testRole, Argv: []string{"--version"}})
		c.Close()
		return rep
	}
	if rep := probe(); rep.OK || !strings.Contains(rep.Error, "image broken") {
		t.Fatalf("%+v", rep)
	}
	if rep := probe(); rep.OK || !strings.Contains(rep.Error, "image broken") {
		t.Fatalf("%+v", rep)
	}
	w.host.mu.Lock()
	n := len(w.host.outputs)
	w.host.mu.Unlock()
	if n != 1 {
		t.Errorf("the image was asked %d times within the window", n)
	}
	time.Sleep(500 * time.Millisecond)
	w.host.mu.Lock()
	w.host.outputErr = nil
	w.host.mu.Unlock()
	if rep := probe(); !rep.OK {
		t.Fatalf("after the window: %+v", rep)
	}
}

// ---- the mounts of the new VM ----

func TestServerRemovesAVMWhoseMountsAreNotWhatWasAskedFor(t *testing.T) {
	for name, bad := range map[string]func([]agent.ContainerMount) []agent.ContainerMount{
		"an extra mount": func(ms []agent.ContainerMount) []agent.ContainerMount {
			return append(ms, agent.ContainerMount{Source: "/Users", Destination: "/host"})
		},
		"a missing mount": func(ms []agent.ContainerMount) []agent.ContainerMount { return ms[:len(ms)-1] },
		"another in-memory mount": func(ms []agent.ContainerMount) []agent.ContainerMount {
			return append(ms, agent.ContainerMount{Source: "tmpfs", Destination: "/var/lib", Type: "tmpfs"})
		},
		"a folder that is not shared from the Mac": func(ms []agent.ContainerMount) []agent.ContainerMount {
			ms[1].Type = "tmpfs" // ms[0] is /tmp
			return ms
		},
		"another source": func(ms []agent.ContainerMount) []agent.ContainerMount {
			ms[1].Source = "/etc"
			return ms
		},
		"read-write instead of read-only": func(ms []agent.ContainerMount) []agent.ContainerMount {
			for i := range ms {
				ms[i].ReadOnly = false
			}
			return ms
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.host.badMounts = bad
			c, rep := w.start(nil)
			defer c.Close()
			if rep.OK || !strings.Contains(rep.Error, "removed") {
				t.Fatalf("%+v", rep)
			}
			_, rm, _ := w.host.snapshot()
			if len(rm) != 1 || w.srv.Sessions() != 0 || w.be.releasedCount() != 1 {
				t.Errorf("removed %q, sessions %d, released %d", rm, w.srv.Sessions(), w.be.releasedCount())
			}
		})
	}
}

// A folder that turns into a link between the check and the run is followed by the tool: the VM is removed.
func TestServerRemovesAVMWhoseFolderBecameALinkWhileItStarted(t *testing.T) {
	w := newWorld(t)
	outside := filepath.Join(filepath.Dir(w.project), "outside")
	os.MkdirAll(outside, 0o700)
	w.host.afterRun = func() {
		os.RemoveAll(w.cwd)
		os.Symlink(outside, w.cwd)
	}
	c, rep := w.start(nil)
	defer c.Close()
	if rep.OK || !strings.Contains(rep.Error, "changed while the VM started") {
		t.Fatalf("%+v", rep)
	}
	if _, rm, _ := w.host.snapshot(); len(rm) != 1 {
		t.Errorf("removed %q", rm)
	}
}

// The image is made ready before any path is resolved: a swap during a long build can't get past the checks.
func TestServerChecksThePathsAfterTheImageIsReady(t *testing.T) {
	w := newWorld(t)
	outside := filepath.Join(filepath.Dir(w.project), "outside2")
	os.MkdirAll(outside, 0o700)
	w.host.ensureHook = func() { // "while the image builds"
		os.RemoveAll(w.cwd)
		os.Symlink(outside, w.cwd)
	}
	c, rep := w.start(nil)
	defer c.Close()
	if rep.OK || !strings.Contains(rep.Error, "not inside the trusted project root") {
		t.Fatalf("%+v", rep)
	}
	if runs, _, _ := w.host.snapshot(); len(runs) != 0 {
		t.Fatal("a VM was run for a folder that had become a link")
	}
}

func TestServerAnswersWithTheContainerToolAsAnAbsolutePath(t *testing.T) {
	w := newWorld(t)
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK || rep.ExecArgv[0] != "/nonexistent-tools/container" {
		t.Fatalf("%+v", rep)
	}
	// a tool that is not on the PATH and is not an absolute name is a refusal
	w2 := newWorld(t, func(o *Options) { o.Host = relativeBin{o.Host.(*fakeHost)} })
	c2, rep2 := w2.start(nil)
	defer c2.Close()
	if rep2.OK || !strings.Contains(rep2.Error, "container tool") {
		t.Fatalf("%+v", rep2)
	}
}

type relativeBin struct{ *fakeHost }

func (relativeBin) ContainerBin() string { return "no-such-container-tool-here" }

func TestServerMakesTheEnvDirectoryPrivate(t *testing.T) {
	w := newWorldWith(t, func(w *world) {
		os.MkdirAll(filepath.Join(w.dir, "t3vm", "env"), 0o755)
	})
	c, rep := w.start(nil)
	defer c.Close()
	if !rep.OK {
		t.Fatalf("%+v", rep)
	}
	if fi, err := os.Stat(filepath.Join(w.dir, "t3vm", "env")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("env dir: %v %v", fi, err)
	}
}
