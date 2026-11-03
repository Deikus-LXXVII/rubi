package t3vm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseArgs(t *testing.T) {
	role, argv, err := ParseArgs([]string{"--role", "r_aaaaaa01", "--", "--model", "m", "--", "x"})
	if err != nil || role != "r_aaaaaa01" || !reflect.DeepEqual(argv, []string{"--model", "m", "--", "x"}) {
		t.Fatalf("%q %q %v", role, argv, err)
	}
	if _, argv, err := ParseArgs([]string{"--role", "r_aaaaaa01", "--"}); err != nil || len(argv) != 0 {
		t.Errorf("no arguments: %q %v", argv, err)
	}
	for _, bad := range [][]string{nil, {"--"}, {"--role"}, {"--role", "r"}, {"--", "--role", "r"}, {"--role", "r", "--other", "--"}, {"x"}} {
		if _, _, err := ParseArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeHome answers one connection of the shim with reply and reports the request and when the connection ended.
type fakeHome struct {
	sock    string
	req     chan StartRequest
	eof     chan time.Time
	replyAt chan time.Time
}

func newFakeHome(t *testing.T, reply StartReply) *fakeHome {
	t.Helper()
	d, err := os.MkdirTemp("", "c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	f := &fakeHome{sock: filepath.Join(d, "s"), req: make(chan StartRequest, 1), eof: make(chan time.Time, 1), replyAt: make(chan time.Time, 1)}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		line, err := br.ReadBytes('\n')
		if err != nil {
			return
		}
		var req StartRequest
		json.Unmarshal(line, &req)
		f.req <- req
		b, _ := json.Marshal(reply)
		c.Write(append(b, '\n'))
		f.replyAt <- time.Now()
		buf := make([]byte, 1)
		for {
			if _, err := br.Read(buf); err != nil {
				f.eof <- time.Now()
				return
			}
		}
	}()
	return f
}

func TestClientSendsTheFolderArgumentsAndEnvironmentThenExecs(t *testing.T) {
	home := newFakeHome(t, StartReply{OK: true, Container: "rubi-t3-x", ExecArgv: []string{"container", "exec", "-i", "rubi-t3-x", "/opt/claude"}})
	var gotPath string
	var gotArgv, gotEnv []string
	var eofEarly bool
	o := ClientOptions{
		Getwd:    func() (string, error) { return "/w/proj", nil },
		Environ:  func() []string { return []string{"A=1", "B=x=y", "PATH=/bin"} },
		LookPath: func(s string) (string, error) { return "/usr/local/bin/" + s, nil },
		Exec: func(path string, argv, env []string, conn *os.File) error {
			gotPath, gotArgv, gotEnv = path, argv, env
			select {
			case <-home.eof:
				eofEarly = true
			case <-time.After(300 * time.Millisecond): // the connection is held open for the process that takes over
			}
			return errors.New("not really")
		},
	}
	var stderr bytes.Buffer
	code := Run(home.sock, "r_aaaaaa01", []string{"--model", "m"}, io.Discard, &stderr, o)
	if code == 0 {
		t.Fatal("a failed exec reported success")
	}
	req := <-home.req
	if req.Op != "session.start" || req.Role != "r_aaaaaa01" || req.Cwd != "/w/proj" || !reflect.DeepEqual(req.Argv, []string{"--model", "m"}) ||
		!reflect.DeepEqual(req.Env, map[string]string{"A": "1", "B": "x=y", "PATH": "/bin"}) {
		t.Fatalf("request: %+v", req)
	}
	if gotPath != "/usr/local/bin/container" || !reflect.DeepEqual(gotArgv, []string{"container", "exec", "-i", "rubi-t3-x", "/opt/claude"}) || len(gotEnv) != 3 {
		t.Fatalf("exec: %q %q %q", gotPath, gotArgv, gotEnv)
	}
	if eofEarly {
		t.Fatal("the connection to Home was closed before the exec")
	}
	// once the process is gone (here: the failed exec returned) the connection is closed
	select {
	case <-home.eof:
	case <-time.After(3 * time.Second):
		t.Fatal("the connection to Home was left open")
	}
	if !strings.Contains(stderr.String(), "can't run container") {
		t.Errorf("stderr: %q", stderr.String())
	}
}

func TestClientNeverRunsClaudeWithoutAVM(t *testing.T) {
	ran := func(*testing.T) ClientOptions {
		return ClientOptions{Getwd: func() (string, error) { return "/w", nil }, Environ: func() []string { return nil },
			Exec: func(string, []string, []string, *os.File) error { t.Error("exec was called"); return errors.New("x") }}
	}
	cases := map[string]struct {
		reply *StartReply
		want  string
	}{
		"refused":                {&StartReply{Error: "this role already runs 1 session(s)"}, "this role already runs 1"},
		"refused no text":        {&StartReply{}, "refused"},
		"ok but no command":      {&StartReply{OK: true}, "no command"},
		"not the container tool": {&StartReply{OK: true, ExecArgv: []string{"/bin/sh", "exec", "-c", "id"}}, "not a container exec"},
		"not exec":               {&StartReply{OK: true, ExecArgv: []string{"/usr/local/bin/container", "run", "-d", "x"}}, "not a container exec"},
		"a lookalike":            {&StartReply{OK: true, ExecArgv: []string{"/tmp/x/container.sh", "exec", "a", "b"}}, "not a container exec"},
		"short command":          {&StartReply{OK: true, ExecArgv: []string{"container"}}, "no command"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := newFakeHome(t, *tc.reply)
			var stderr bytes.Buffer
			if code := Run(home.sock, "r", nil, io.Discard, &stderr, ran(t)); code == 0 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
		})
	}
	t.Run("home not running", func(t *testing.T) {
		var stderr bytes.Buffer
		if code := Run(filepath.Join(t.TempDir(), "none"), "r", nil, io.Discard, &stderr, ran(t)); code == 0 || !strings.Contains(stderr.String(), "isn't reachable") {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
	})
	t.Run("container tool missing", func(t *testing.T) {
		home := newFakeHome(t, StartReply{OK: true, ExecArgv: []string{"container", "exec", "x", "y"}})
		o := ran(t)
		o.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		var stderr bytes.Buffer
		if code := Run(home.sock, "r", nil, io.Discard, &stderr, o); code == 0 || !strings.Contains(stderr.String(), "can't find container") {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		select {
		case <-home.eof:
		case <-time.After(3 * time.Second):
			t.Fatal("the connection was left open, so Home keeps a VM for nothing")
		}
	})
	t.Run("garbage answer", func(t *testing.T) {
		d, _ := os.MkdirTemp("", "c")
		defer os.RemoveAll(d)
		sock := filepath.Join(d, "s")
		ln, _ := net.Listen("unix", sock)
		defer ln.Close()
		go func() {
			c, _ := ln.Accept()
			bufio.NewReader(c).ReadBytes('\n')
			c.Write([]byte("not json\n"))
			c.Close()
		}()
		var stderr bytes.Buffer
		if code := Run(sock, "r", nil, io.Discard, &stderr, ran(t)); code == 0 || !strings.Contains(stderr.String(), "wasn't understood") {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
	})
}

// The real exec: the process becomes the command, keeps Home's connection open for as long as it runs, exits with
// the command's status, and the connection ends with it.
func TestClientRealExecKeepsTheConnectionUntilTheProcessEnds(t *testing.T) {
	if os.Getenv("T3VM_CLIENT_HELPER") == "1" {
		role, argv, _ := ParseArgs(strings.Split(os.Getenv("T3VM_CLIENT_ARGS"), " "))
		os.Exit(Run(os.Getenv("T3VM_CLIENT_SOCK"), role, argv, os.Stdout, os.Stderr, ClientOptions{}))
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	fake := filepath.Join(t.TempDir(), "container") // stands in for the container tool: sleeps, then exits 7
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 0.4\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := newFakeHome(t, StartReply{OK: true, Container: "c", ExecArgv: []string{fake, "exec", "-i", "c", "claude"}})
	cmd := exec.Command(os.Args[0], "-test.run=^TestClientRealExecKeepsTheConnectionUntilTheProcessEnds$")
	cmd.Env = append(os.Environ(), "T3VM_CLIENT_HELPER=1", "T3VM_CLIENT_ARGS=--role r_aaaaaa01 -- --verbose", "T3VM_CLIENT_SOCK="+home.sock)
	err := cmd.Run()
	end := time.Now()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the shim's exit status: %v (want the command's 7)", err)
	}
	replyAt := <-home.replyAt
	eofAt := <-home.eof
	if d := eofAt.Sub(replyAt); d < 300*time.Millisecond {
		t.Fatalf("the connection closed %s after the answer: it was not held for the command's lifetime", d)
	}
	if end.Sub(eofAt) > 3*time.Second {
		t.Errorf("the connection outlived the process by %s", end.Sub(eofAt))
	}
	if req := <-home.req; req.Role != "r_aaaaaa01" || !reflect.DeepEqual(req.Argv, []string{"--verbose"}) || req.Cwd == "" {
		t.Errorf("request: %+v", req)
	}
}

// `--version` alone is answered by Home and printed; no session is asked for, nothing is executed.
func TestClientAsksHomeForTheVersionAndStartsNothing(t *testing.T) {
	home := newFakeHome(t, StartReply{OK: true, Output: "2.1.314 (Claude Code)\n"})
	var out, errb bytes.Buffer
	o := ClientOptions{Getwd: func() (string, error) { return "/w", nil }, Environ: func() []string { return []string{"SECRET=1"} },
		Exec: func(string, []string, []string, *os.File) error { t.Error("exec was called"); return nil }}
	if code := Run(home.sock, "r_aaaaaa01", []string{"--version"}, &out, &errb, o); code != 0 || out.String() != "2.1.314 (Claude Code)\n" {
		t.Fatalf("exit %d, out %q, err %q", code, out.String(), errb.String())
	}
	req := <-home.req
	if req.Op != OpVersion || req.Env != nil || !reflect.DeepEqual(req.Argv, []string{"--version"}) {
		t.Fatalf("request: %+v", req)
	}
	// anything more than --version is a session
	home2 := newFakeHome(t, StartReply{Error: "no"})
	Run(home2.sock, "r_aaaaaa01", []string{"--version", "--verbose"}, &out, &errb, o)
	if req := <-home2.req; req.Op != "session.start" {
		t.Fatalf("request: %+v", req)
	}
	// an empty answer is a failure, not a blank version
	home3 := newFakeHome(t, StartReply{OK: true})
	out.Reset()
	if code := Run(home3.sock, "r_aaaaaa01", []string{"--version"}, &out, &errb, o); code == 0 || out.Len() != 0 {
		t.Fatalf("exit %d, out %q", code, out.String())
	}
}

func TestClientUsesTheAbsoluteContainerPathWithoutLookingItUp(t *testing.T) {
	home := newFakeHome(t, StartReply{OK: true, ExecArgv: []string{"/opt/tools/container", "exec", "-i", "c", "claude"}})
	var gotPath string
	o := ClientOptions{Getwd: func() (string, error) { return "/w", nil }, Environ: func() []string { return nil },
		LookPath: func(string) (string, error) { t.Error("the PATH was searched"); return "/elsewhere/container", nil },
		Exec:     func(path string, argv, env []string, conn *os.File) error { gotPath = path; return errors.New("stop") }}
	Run(home.sock, "r", nil, io.Discard, io.Discard, o)
	if gotPath != "/opt/tools/container" {
		t.Fatalf("ran %q", gotPath)
	}
}
