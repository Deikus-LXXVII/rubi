package guest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// Server is the guest's side: it serves Home's streams on the control channel.
type Server struct {
	// Mount mounts one shared folder (the Linux one: mkdir and mount -t virtiofs).
	Mount func(Mount) error
	// PowerOff ends the VM; it is called after the answer was written.
	PowerOff func() error
	// Prepare readies a command before it starts: the Linux one makes it run as req's user in its own session and
	// process group (Kill then ends the group). Nil leaves it as it is (tests).
	Prepare func(cmd *exec.Cmd, req ExecRequest)
	// Kill ends a started command and what it started (nil: the process alone).
	Kill func(cmd *exec.Cmd)
	// AwaitExit waits until the command's leader has exited without reaping it (Linux: waitid with WNOWAIT), so that
	// Kill can still end its group by the leader's id; nil skips that step.
	AwaitExit func(cmd *exec.Cmd)
	// Logf gets what goes wrong (nil discards).
	Logf func(format string, args ...any)

	wg sync.WaitGroup
}

// Serve serves the channel until it ends: it says HELLO once it is ready, then answers each stream Home opens.
func (s *Server) Serve(rw io.ReadWriteCloser) error {
	mux := netpipe.New(rw, netpipe.Config{
		OnOpen: func(svc byte, st *netpipe.Stream) bool {
			switch svc {
			case SvcControl:
				s.wg.Add(1)
				go func() { defer s.wg.Done(); s.control(st) }()
			case SvcExec:
				s.wg.Add(1)
				go func() { defer s.wg.Done(); s.exec(st) }()
			default:
				return false
			}
			return true
		},
		Logf: s.Logf,
	})
	if err := mux.SendHello(); err != nil {
		return err
	}
	err := mux.Run()
	s.wg.Wait()
	return err
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// control answers one control request per line until Home closes the stream.
func (s *Server) control(st *netpipe.Stream) {
	defer st.Close()
	for {
		line, err := readLine(st)
		if err != nil {
			return
		}
		var req ControlRequest
		var rep ControlReply
		after := func() {}
		if err := json.Unmarshal(line, &req); err != nil {
			rep.Error = "not a request"
		} else {
			switch req.Op {
			case "ping":
			case "mount":
				rep.Error = s.mountAll(req.Mounts)
			case "poweroff":
				if s.PowerOff == nil {
					rep.Error = "can't power off"
				} else {
					after = func() {
						if err := s.PowerOff(); err != nil {
							s.logf("guest: power off: %v", err)
						}
					}
				}
			default:
				rep.Error = fmt.Sprintf("unknown op %q", req.Op)
			}
		}
		if err := writeLine(st, rep); err != nil {
			return
		}
		after()
	}
}

func (s *Server) mountAll(ms []Mount) string {
	if s.Mount == nil {
		return "can't mount"
	}
	for _, m := range ms {
		if !strings.HasPrefix(m.Target, "/") || m.Target == "/" || path.Clean(m.Target) != m.Target || m.Tag == "" {
			return fmt.Sprintf("can't mount %q at %q", m.Tag, m.Target)
		}
		if err := s.Mount(m); err != nil {
			return fmt.Sprintf("mounting %s at %s: %v", m.Tag, m.Target, err)
		}
	}
	return ""
}

// command checks req and makes its command; the error is said to Home.
func command(req ExecRequest) (*exec.Cmd, error) {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return nil, errors.New("no program")
	}
	if !validID(req.UID) || !validID(req.GID) {
		return nil, errors.New("a process doesn't run as root (a user and group from 1 to 4294967294)")
	}
	if !path.IsAbs(req.Dir) {
		return nil, errors.New("the folder must be an absolute path")
	}
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("environment variable %q", k)
		}
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	prog := req.Argv[0]
	if !strings.Contains(prog, "/") {
		found := ""
		for _, dir := range strings.Split(req.Env["PATH"], ":") {
			if dir == "" || !path.IsAbs(dir) {
				continue
			}
			if p := path.Join(dir, prog); isExecutable(p) {
				found = p
				break
			}
		}
		if found == "" {
			return nil, fmt.Errorf("%s: not found in PATH", prog)
		}
		prog = found
	} else if !path.IsAbs(prog) {
		return nil, errors.New("the program must be an absolute path or a name in PATH")
	}
	cmd := &exec.Cmd{Path: prog, Args: req.Argv, Env: env, Dir: req.Dir}
	return cmd, nil
}

// exec runs one process for one stream.
//
// What ends it: the process's leader exiting (then what it started is killed with it: Kill ends the group), or Home
// closing the stream or the channel ending (PeerDone; then it is killed at once, whether or not anything reads the
// stream: a process that ignores its stdin can't keep the reader stuck). Output still in its pipes after the leader's
// exit is passed on for at most outputGrace, so a descendant that escaped the group and keeps the pipes open can't hold
// the stream.
func (s *Server) exec(st *netpipe.Stream) {
	defer st.Close()
	line, err := readLine(st)
	if err != nil {
		return
	}
	var req ExecRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeLine(st, ExecStarted{Error: "not a request"})
		return
	}
	cmd, err := command(req)
	if err != nil {
		_ = writeLine(st, ExecStarted{Error: err.Error()})
		return
	}
	if s.Prepare != nil {
		s.Prepare(cmd, req)
	}
	// The pipes are the guest agent's own files (not exec's copying goroutines), so that Wait returns when the leader
	// exits, whoever else holds them.
	inR, inW, err1 := os.Pipe()
	outR, outW, err2 := os.Pipe()
	errR, errW, err3 := os.Pipe()
	closeAll := func(fs ...*os.File) {
		for _, f := range fs {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	if err := errors.Join(err1, err2, err3); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		_ = writeLine(st, ExecStarted{Error: err.Error()})
		return
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		_ = writeLine(st, ExecStarted{Error: err.Error()})
		return
	}
	closeAll(inR, outW, errW) // the child's ends

	// kill ends the process (and its group) while its leader has not been reaped; after that its pid may be another's.
	var kmu sync.Mutex
	reaped := false
	kill := func() {
		kmu.Lock()
		defer kmu.Unlock()
		if reaped {
			return
		}
		if s.Kill != nil {
			s.Kill(cmd)
		} else {
			_ = cmd.Process.Kill()
		}
	}
	var wmu sync.Mutex // one chunk at a time on the stream
	send := func(kind byte, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeChunks(st, kind, b)
	}
	if err := writeLine(st, ExecStarted{}); err != nil {
		kill()
		closeAll(inW, outR, errR)
		_ = cmd.Wait()
		return
	}
	leaderDone := make(chan struct{})
	// Home leaving kills the process, whatever the reader below is doing.
	go func() {
		select {
		case <-st.PeerDone():
			kill()
		case <-leaderDone:
		}
	}()
	// Home's side: stdin and its end. A write to a process that doesn't read blocks only this goroutine.
	go func() {
		defer inW.Close()
		for {
			kind, b, err := readChunk(st)
			if err != nil {
				return
			}
			switch kind {
			case ChunkStdin:
				_, _ = inW.Write(b) // a process that closed its stdin: keep reading, the stream's end is still to come
			case ChunkStdinEOF:
				_ = inW.Close()
			default:
				kill()
				return
			}
		}
	}()
	var pumps sync.WaitGroup
	pump := func(r *os.File, kind byte) {
		defer pumps.Done()
		buf := make([]byte, MaxChunk)
		for {
			n, err := r.Read(buf)
			if n > 0 && send(kind, buf[:n]) != nil {
				kill()
				return
			}
			if err != nil {
				return
			}
		}
	}
	pumps.Add(2)
	go pump(outR, ChunkStdout)
	go pump(errR, ChunkStderr)

	// The leader's end: what it started goes with it, then the leader is reaped.
	if s.AwaitExit != nil {
		s.AwaitExit(cmd) // exited, not yet reaped: the group still has its id
		kill()
	}
	_ = cmd.Wait()
	kmu.Lock()
	reaped = true
	kmu.Unlock()
	close(leaderDone)
	pumped := make(chan struct{})
	go func() { pumps.Wait(); close(pumped) }()
	select {
	case <-pumped:
	case <-time.After(outputGrace):
		closeAll(outR, errR) // a descendant outside the group still holds the pipes
		<-pumped
	}
	closeAll(outR, errR)
	code := int32(cmd.ProcessState.ExitCode())
	_ = send(ChunkExit, []byte{byte(code >> 24), byte(code >> 16), byte(code >> 8), byte(code)})
	_ = st.CloseWrite()
}

// outputGrace is how long output left in the pipes is passed on after the leader's exit.
var outputGrace = 2 * time.Second
