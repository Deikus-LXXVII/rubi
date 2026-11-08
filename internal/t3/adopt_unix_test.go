//go:build linux || darwin

package t3

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// orphan starts a helper server and returns a second value with the same settings, as the next run of Rubi
// Home would make after a crash left the first one's server running.
func orphan(t *testing.T, mode string) (first, next *Server) {
	t.Helper()
	first = helperServer(t)
	if mode != "" {
		_ = os.WriteFile(filepath.Join(first.BaseDir, "mode"), []byte(mode), 0o600)
	}
	if _, err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	next = &Server{Node: first.Node, Bin: first.Bin, BaseDir: first.BaseDir, Port: first.Port, AdoptPoll: 50 * time.Millisecond}
	t.Cleanup(next.Stop)
	return first, next
}

func TestAdoptTakesOverALeftServer(t *testing.T) {
	first, next := orphan(t, "grandchild")
	child := readPid(t, filepath.Join(first.BaseDir, "child.pid"))
	v, ok, err := next.Adopt(context.Background())
	if err != nil || !ok || v != "9.9.9-helper" {
		t.Fatalf("Adopt = %q, %v, %v", v, ok, err)
	}
	if !next.Alive() || !next.Adopted() || next.Pid() != first.Pid() || next.Version() != v {
		t.Errorf("adopted server: alive %v adopted %v pid %d (want %d) version %q", next.Alive(), next.Adopted(), next.Pid(), first.Pid(), next.Version())
	}
	if _, _, err := next.Adopt(context.Background()); err == nil {
		t.Error("a second Adopt over a live adopted server is accepted")
	}
	next.Stop()
	select {
	case <-first.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("Stop of the adopted server left it running")
	}
	waitDead(t, child, "the adopted server's child")
	if next.Alive() || next.Pid() != 0 {
		t.Error("the stopped adopted server still counts as running")
	}
}

func TestAdoptedServerThatDiesIsNoticed(t *testing.T) {
	first, next := orphan(t, "")
	if _, ok, err := next.Adopt(context.Background()); !ok || err != nil {
		t.Fatalf("Adopt: %v %v", ok, err)
	}
	done := next.Done()
	killServer(t, first)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the adopted server's end was not noticed")
	}
	if next.Alive() {
		t.Error("a dead adopted server counts as alive")
	}
	next.Stop() // signals nothing, doesn't hang
	if _, err := next.Start(context.Background()); err != nil {
		t.Errorf("start after the adopted server died: %v", err)
	}
}

func TestAdoptNothingToTakeOver(t *testing.T) {
	s := &Server{Node: os.Args[0], Bin: "-test.run=^TestHelperProcess$", BaseDir: t.TempDir(), Port: freePort(t)}
	if v, ok, err := s.Adopt(context.Background()); ok || err != nil || v != "" {
		t.Errorf("Adopt with no runtime file = %q %v %v", v, ok, err)
	}
	if s.Alive() {
		t.Error("alive after adopting nothing")
	}
	if _, _, err := (&Server{}).Adopt(context.Background()); err == nil {
		t.Error("Adopt without settings is accepted")
	}
}

func TestAdoptRefusesAnotherServersProcess(t *testing.T) {
	cases := map[string]func(s *Server){
		"other bin":  func(s *Server) { s.Bin = "-test.run=^TestHelperProcessX$" },
		"other node": func(s *Server) { s.Node = s.Node + "-other" },
		"other port": func(s *Server) { s.Port++ },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			first, next := orphan(t, "")
			change(next)
			if _, ok, _ := next.Adopt(context.Background()); ok {
				t.Error("adopted a process that is not this server")
			}
			if !first.Alive() {
				t.Error("a refused adoption stopped the other process")
			}
		})
	}
}

func TestAdoptRefusesAnUnrelatedRuntimePid(t *testing.T) {
	s := &Server{Node: os.Args[0], Bin: "-test.run=^TestHelperProcess$", BaseDir: t.TempDir(), Port: freePort(t)}
	t.Cleanup(s.Stop)
	pid := sibling(t)
	_ = os.MkdirAll(filepath.Join(s.BaseDir, "userdata"), 0o700)
	_ = os.WriteFile(s.runtimeFile(), mustJSON(map[string]any{"pid": pid, "port": s.Port, "serverVersion": "1"}), 0o600)
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(s.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ok, _ := s.Adopt(context.Background()); ok {
		t.Error("adopted a process that only the runtime file names")
	}
	if !alive(pid) {
		t.Error("the unrelated process was signalled")
	}
}

func TestAdoptStopsALeftServerThatDoesNotAnswer(t *testing.T) {
	first, next := orphan(t, "")
	_ = os.WriteFile(filepath.Join(first.BaseDir, "deaf"), nil, 0o600)
	deadline := time.Now().Add(5 * time.Second)
	for portAnswers(first.Port) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok, err := next.Adopt(context.Background()); ok || err != nil {
		t.Fatalf("Adopt of a hung server = %v %v", ok, err)
	}
	select {
	case <-first.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("a left server that doesn't answer was not stopped")
	}
	if next.Alive() {
		t.Error("the hung server counts as adopted")
	}
}

func portAnswers(port int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func TestAdoptNeedsAServerVersion(t *testing.T) {
	first, next := orphan(t, "")
	_ = os.WriteFile(first.runtimeFile(), mustJSON(map[string]any{"pid": first.Pid(), "port": first.Port}), 0o600)
	if _, ok, _ := next.Adopt(context.Background()); ok {
		t.Error("adopted a server whose runtime file names no version")
	}
}

func TestIsServerNeedsTheGroupLeaderSeenAtAdoption(t *testing.T) {
	s := &Server{Node: os.Args[0], Bin: "-test.run=^TestHelperProcess$", BaseDir: t.TempDir(), Port: freePort(t)}
	// The same serve line, but in the test's process group: stopping its "group" would reach other processes.
	cmd := exec.Command(s.Node, append([]string{s.Bin}, s.serveArgs()...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if s.isServer(cmd.Process.Pid, 0) {
		t.Error("a process that doesn't lead its own group counts as the server")
	}
	first, _ := orphan(t, "")
	st, err := procOf(first.Pid())
	if err != nil {
		t.Fatal(err)
	}
	if !first.isServer(first.Pid(), st.Start) {
		t.Fatal("the started server isn't recognised")
	}
	if first.isServer(first.Pid(), st.Start+1) {
		t.Error("a process with another start time (an id given to a later process) counts as the server")
	}
}

func TestStopAdoptedSignalsNothingOnceTheIdIsSomeoneElses(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	own(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	stopAdopted(cmd.Process.Pid, make(chan struct{}), func() bool { return false })
	time.Sleep(200 * time.Millisecond)
	if gone(cmd.Process.Pid) {
		t.Error("stopAdopted signalled a group whose leader is no longer the server")
	}
}

func TestProcOfAMissingProcess(t *testing.T) {
	if _, err := procOf(1 << 30); err == nil {
		t.Error("a missing process reads as one")
	}
}

// killLeader kills only the server's leader (the launcher), as a crash of it would, and waits for s to notice.
func killLeader(t *testing.T, s *Server) {
	t.Helper()
	done := s.Done()
	_ = syscall.Kill(s.Pid(), syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's end was not noticed")
	}
}

func TestStopKillsTheRuntimeTheLeaderLeft(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("runtime-child"), 0o600)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := readPid(t, filepath.Join(s.BaseDir, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	killLeader(t, s)
	if gone(child) {
		t.Fatal("the runtime died with the leader; the test shows nothing")
	}
	s.Stop()
	waitDead(t, child, "the runtime the leader left")
}

func TestStopLeavesAGroupThatIsntTheServers(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("grandchild"), 0o600) // a child that doesn't name the base dir
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := readPid(t, filepath.Join(s.BaseDir, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	killLeader(t, s)
	s.Stop()
	time.Sleep(200 * time.Millisecond)
	if gone(child) {
		t.Error("a group with no member naming the base dir was killed after its leader died")
	}
}

func TestAdoptClearsTheRuntimeOfADeadLauncher(t *testing.T) {
	first, next := orphan(t, "runtime-child")
	child := readPid(t, filepath.Join(first.BaseDir, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	killLeader(t, first)
	if _, ok, err := next.Adopt(context.Background()); ok || err != nil {
		t.Fatalf("Adopt with the launcher dead = %v %v", ok, err)
	}
	waitDead(t, child, "the runtime of a dead launcher")
}

func TestNamesBase(t *testing.T) {
	if !namesBase([]string{"t3", "serve", "--base-dir", "/b", "--port", "1"}, "/b") {
		t.Error("the base dir isn't found")
	}
	for _, argv := range [][]string{{"t3", "--base-dir"}, {"t3", "--base-dir", "/b2"}, {"t3", "/b"}, {"--base-dir=/b"}} {
		if namesBase(argv, "/b") {
			t.Errorf("%q names /b", argv)
		}
	}
}
