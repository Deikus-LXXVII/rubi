//go:build unix

package t3

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests run the test binary itself as "node": TestHelperProcess plays the t3 bin when it is given the
// arguments the real one would get. A file named "mode" in the base dir picks what it does.

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestHelperProcess(t *testing.T) {
	args := os.Args
	var cmd string
	for i, a := range args {
		if a == "serve" || a == "auth" {
			cmd, args = a, args[i:]
			break
		}
	}
	if cmd == "" {
		t.Skip("helper for the server tests")
	}
	dir := argAfter(args, "--base-dir")
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	if cmd == "auth" {
		_ = os.WriteFile(filepath.Join(dir, "auth-args.json"), mustJSON(args), 0o600)
		if string(mode) == "fail" {
			os.Stdout.WriteString(`{"credential":"LEAKY-CODE"}`)
			os.Stderr.WriteString("boom")
			os.Exit(1)
		}
		os.Stdout.WriteString(`{"id":"x","credential":"CODE-123","label":"l"}`)
		os.Exit(0)
	}
	_ = os.WriteFile(filepath.Join(dir, "env.txt"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "args.json"), mustJSON(args), 0o600)
	port := argAfter(args, "--port")
	switch string(mode) {
	case "noisy":
		os.Stdout.WriteString("Pairing link: http://127.0.0.1/pair#token=FAKE-QR-TOKEN\n")
		os.Stderr.WriteString("QR FAKE-QR-BLOCK\n")
		os.Exit(3)
	case "grandchild", "detached":
		gc := exec.Command("/bin/sleep", "60")
		if string(mode) == "detached" {
			// Out of the group, holding the server's output open.
			gc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			gc.Stdout, gc.Stderr = os.Stdout, os.Stderr
		}
		if err := gc.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(gc.Process.Pid)), 0o600)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(2)
	}
	time.Sleep(300 * time.Millisecond)
	pid := os.Getpid()
	if string(mode) == "sibling-pid" {
		b, _ := os.ReadFile(filepath.Join(dir, "sibling.pid"))
		pid, _ = strconv.Atoi(string(b))
	}
	p, _ := strconv.Atoi(port)
	_ = os.MkdirAll(filepath.Join(dir, "userdata"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "userdata", "server-runtime.json"),
		mustJSON(map[string]any{"version": 1, "pid": pid, "port": p, "serverVersion": "9.9.9-helper"}), 0o600)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	time.Sleep(time.Minute)
	os.Exit(0)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func helperServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{Node: os.Args[0], Bin: "-test.run=^TestHelperProcess$", BaseDir: t.TempDir(), Port: freePort(t)}
	t.Cleanup(s.Stop)
	return s
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestServerEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("ANTHROPIC_API_KEY", "must-not-pass")
	t.Setenv("SECRET_THING", "must-not-pass")
	s := helperServer(t)
	want := []string{
		"PATH=" + filepath.Dir(os.Args[0]) + ":/usr/bin:/bin",
		"HOME=/home/someone",
		"T3CODE_TELEMETRY_ENABLED=false",
		"OTEL_SDK_DISABLED=true",
	}
	if got := s.Env(); !reflect.DeepEqual(got, want) {
		t.Errorf("Env() = %v", got)
	}
	// A runtime home (where Rubi Home keeps the checked T3 executable) adds exactly one variable.
	s.RuntimeHome = "/rt/home"
	if got := s.Env(); !reflect.DeepEqual(got, append(append([]string{}, want...), "T3CODE_HOME=/rt/home")) {
		t.Errorf("Env() with a runtime home = %v", got)
	}
	s.RuntimeHome = ""
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.BaseDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(b), "\n")
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the server saw %v, want %v", got, want)
	}
}

func TestServerStartAndStop(t *testing.T) {
	s := helperServer(t)
	// A runtime file left by an earlier run must not be taken for this one.
	stale := filepath.Join(s.BaseDir, "userdata", "server-runtime.json")
	_ = os.MkdirAll(filepath.Dir(stale), 0o700)
	_ = os.WriteFile(stale, mustJSON(map[string]any{"pid": os.Getpid(), "port": s.Port, "serverVersion": "stale"}), 0o600)

	v, err := s.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "9.9.9-helper" || s.Version() != v {
		t.Errorf("version = %q", v)
	}
	var args []string
	b, _ := os.ReadFile(filepath.Join(s.BaseDir, "args.json"))
	_ = json.Unmarshal(b, &args)
	for _, a := range []string{"serve", "--no-browser"} {
		if !contains(args, a) {
			t.Errorf("args %v lack %s", args, a)
		}
	}
	if argAfter(args, "--host") != "127.0.0.1" || argAfter(args, "--port") != strconv.Itoa(s.Port) || argAfter(args, "--base-dir") != s.BaseDir {
		t.Errorf("args = %v", args)
	}
	if _, err := s.Start(context.Background()); err == nil {
		t.Error("started twice")
	}

	var ri runtimeInfo
	rb, _ := os.ReadFile(stale)
	_ = json.Unmarshal(rb, &ri)
	s.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for alive(ri.PID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(ri.PID) {
		t.Error("the server is still running after Stop")
	}
	if _, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), time.Second); err == nil {
		t.Error("the port still answers after Stop")
	}
	if _, err := s.Start(context.Background()); err != nil {
		t.Errorf("restart: %v", err)
	}
}

// sibling starts a process that is not a child of the server, and returns its pid.
func sibling(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

func TestServerRefusesAnUnrelatedRuntimePid(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("sibling-pid"), 0o600)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "sibling.pid"), []byte(strconv.Itoa(sibling(t))), 0o600)
	if _, err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("err = %v", err)
	}
	if s.Version() != "" || s.Alive() {
		t.Error("a refused server is kept")
	}
}

func TestOurs(t *testing.T) {
	a, b := sibling(t), sibling(t)
	if !ours(a, a) || !ours(a, os.Getpid()) {
		t.Error("the process itself, or a child, does not count as ours")
	}
	if ours(a, b) || ours(os.Getpid(), a) {
		t.Error("an unrelated process counts as ours")
	}
}

func TestStopKillsTheGroup(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("grandchild"), 0o600)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := readPid(t, filepath.Join(s.BaseDir, "child.pid"))
	if !alive(child) {
		t.Fatal("the helper's child is not running")
	}
	s.Stop()
	waitDead(t, child, "the server's child")
}

func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// gone reports whether a process has ended; an unreaped zombie counts as ended.
func gone(pid int) bool {
	out, _ := exec.Command("/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	return st == "" || strings.HasPrefix(st, "Z")
}

func waitDead(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if gone(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s is still running", what)
}

func TestStopDoesNotHangOnADetachedChild(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("detached"), 0o600)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	child := readPid(t, filepath.Join(s.BaseDir, "child.pid"))
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Stop hangs while a detached child keeps the server's output open")
	}
}

func TestStopAfterTheServerDiedSignalsNothing(t *testing.T) {
	// A process that leads its own group but is "already reaped" as far as the server knows.
	cmd := exec.Command("/bin/sleep", "60")
	own(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	done := make(chan struct{})
	close(done)
	stop(cmd, done)
	time.Sleep(200 * time.Millisecond)
	if gone(cmd.Process.Pid) {
		t.Error("stop signalled a group whose leader was already reaped")
	}
}

// killServer kills the helper the way a crash would.
func killServer(t *testing.T, s *Server) {
	t.Helper()
	var ri runtimeInfo
	b, _ := os.ReadFile(filepath.Join(s.BaseDir, "userdata", "server-runtime.json"))
	_ = json.Unmarshal(b, &ri)
	done := s.Done()
	_ = syscall.Kill(ri.PID, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed")
	}
}

func TestStartAfterACrash(t *testing.T) {
	s := helperServer(t)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Alive() {
		t.Fatal("not alive after Start")
	}
	if s.Pid() <= 1 {
		t.Errorf("pid %d of a running server", s.Pid())
	}
	killServer(t, s)
	if s.Alive() {
		t.Error("alive after the process died")
	}
	if s.Pid() != 0 {
		t.Errorf("pid %d of a dead server", s.Pid())
	}
	s.Stop() // must not signal or hang
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("start after a crash: %v", err)
	}
	// A crash with no Stop in between.
	killServer(t, s)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("start over a dead server: %v", err)
	}
	if !s.Alive() {
		t.Error("not alive after the second start")
	}
}

func TestStartErrorHoldsNoServerOutput(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("noisy"), 0o600)
	_, err := s.Start(context.Background())
	if err == nil {
		t.Fatal("started")
	}
	if strings.Contains(err.Error(), "FAKE-QR") {
		t.Errorf("error holds the server's output: %v", err)
	}
	if !strings.Contains(err.Error(), s.LogFile()) {
		t.Errorf("error does not name the log file: %v", err)
	}
	st, rerr := os.Stat(s.LogFile())
	if rerr != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("log file: %v, %v", st, rerr)
	}
	if b, _ := os.ReadFile(s.LogFile()); !strings.Contains(string(b), "FAKE-QR-TOKEN") {
		t.Error("the output was not kept in the log file")
	}
}

func TestStartTightensTheBaseDir(t *testing.T) {
	s := helperServer(t)
	if err := os.Chmod(s.BaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(s.BaseDir); st.Mode().Perm() != 0o700 {
		t.Errorf("base dir mode = %v", st.Mode().Perm())
	}
}

func TestServerStartNeedsSettings(t *testing.T) {
	if _, err := (&Server{}).Start(context.Background()); err == nil {
		t.Error("an empty server started")
	}
}

func TestPairingCode(t *testing.T) {
	s := helperServer(t)
	code, err := s.PairingCode(context.Background(), PairingScopes("full-access"), 10*time.Minute, "Rubi Home")
	if err != nil || code != "CODE-123" {
		t.Fatalf("code = %q, %v", code, err)
	}
	var args []string
	b, _ := os.ReadFile(filepath.Join(s.BaseDir, "auth-args.json"))
	_ = json.Unmarshal(b, &args)
	var scopes []string
	for i, a := range args {
		if a == "--scope" {
			scopes = append(scopes, args[i+1])
		}
	}
	if !reflect.DeepEqual(scopes, PairingScopes("full-access")) {
		t.Errorf("scopes = %v", scopes)
	}
	if argAfter(args, "--ttl") != "10m" || argAfter(args, "--label") != "Rubi Home" || argAfter(args, "--base-dir") != s.BaseDir || !contains(args, "--json") {
		t.Errorf("args = %v", args)
	}
}

func TestPairingCodeFailureDoesNotLeak(t *testing.T) {
	s := helperServer(t)
	_ = os.WriteFile(filepath.Join(s.BaseDir, "mode"), []byte("fail"), 0o600)
	_, err := s.PairingCode(context.Background(), []string{"orchestration:read"}, time.Minute, "l")
	if err == nil {
		t.Fatal("a failing command gave a code")
	}
	if strings.Contains(err.Error(), "LEAKY-CODE") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

func TestTTLArg(t *testing.T) {
	for d, want := range map[time.Duration]string{10 * time.Minute: "10m", time.Minute: "1m", 90 * time.Second: "90s",
		30 * time.Second: "30s", 0: "1s", time.Hour: "60m"} {
		if got := ttlArg(d); got != want {
			t.Errorf("ttlArg(%v) = %q, want %q", d, got, want)
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestServerStaysApartFromTheUsersT3(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	user := filepath.Join(home, ".t3")
	if err := os.MkdirAll(filepath.Join(user, "userdata"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "elsewhere")
	if err := os.Symlink(user, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{user, filepath.Join(user, "rubi"), link, filepath.Join(link, "x", "y"), filepath.Join(user, "..", ".t3"), filepath.Join(user, "..x")} {
		s := helperServer(t)
		s.BaseDir = dir
		if _, err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "own T3 home") {
			t.Errorf("base dir %s: err = %v", dir, err)
		}
	}
	s := helperServer(t)
	s.Port = UserPort
	if _, err := s.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "port 3773") {
		t.Errorf("port 3773: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(user, "userdata", "server.log")); err == nil {
		t.Error("something was written into the user's T3 home")
	}
	// A folder next to it, even one whose name starts the same, is fine.
	s = helperServer(t)
	s.BaseDir = filepath.Join(home, ".t3-rubi")
	if _, err := s.Start(context.Background()); err != nil {
		t.Errorf("own home: %v", err)
	}
}
