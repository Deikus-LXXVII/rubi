package t3vm

// Self-test support (t3-setup.run selftest): the same VM a session gets, started for Home's own use. Probe starts it
// through the session path (the same role checks, caps, mounts, proxy and hook gate), runs a command in it instead of
// claude and removes it; Version asks the image for claude's version as the shim's first call does.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// probeLimit bounds a probe VM's whole life.
var probeLimit = 5 * time.Minute // a var for tests

// Version is claude's version in the image, asked as `claude --version` through a shim is (OpVersion).
func (s *Server) Version(ctx context.Context, role string) (string, error) {
	return s.version(ctx, StartRequest{Op: OpVersion, Role: role, Argv: []string{"--version"}})
}

// Probe starts a VM for the role exactly as a session does, in the role's project folder, runs cmd in it with the
// session's environment and returns what it printed. The VM is gone when Probe returns.
func (s *Server) Probe(ctx context.Context, roleID string, cmd []string) ([]byte, error) {
	if len(cmd) == 0 {
		return nil, errors.New("t3vm: nothing to run")
	}
	role, err := s.opt.Backend.T3Role(roleID)
	if err != nil {
		return nil, err
	}
	dir := role.ProjectDir
	role.Release()
	ctx, cancel := context.WithTimeout(ctx, probeLimit)
	defer cancel()
	if s.opt.Stream != nil {
		return s.probeStream(ctx, cancel, roleID, dir, cmd)
	}
	ses, rep, err := s.start(ctx, cancel, StartRequest{Op: "session.start", Role: roleID, Cwd: dir})
	if err != nil {
		return nil, err
	}
	defer ses.teardown()
	i := slices.Index(rep.ExecArgv, s.opt.Claude)
	if i < 0 {
		return nil, errors.New("t3vm: the exec line has no claude in it")
	}
	return s.opt.Host.RunOutput(ctx, append(slices.Clone(rep.ExecArgv[:i]), cmd...))
}

// probeStream is Probe for streamed sessions: the session's VM, with cmd run where claude would be.
func (s *Server) probeStream(ctx context.Context, cancel func(), roleID, dir string, cmd []string) ([]byte, error) {
	ses, err := s.startStream(ctx, cancel, StartRequest{Op: "session.start", Role: roleID, Cwd: dir})
	if err != nil {
		return nil, err
	}
	defer ses.teardown()
	st := ses.stream
	var stderr bytes.Buffer
	p, err := st.vm.ExecIn(ctx, st.dir, st.env, cmd, &limited{w: &stderr, n: 4096})
	if err != nil {
		return nil, err
	}
	defer p.Close()
	stop := context.AfterFunc(ctx, func() { _ = p.Close() })
	defer stop()
	_ = p.CloseWrite()
	out, err := io.ReadAll(io.LimitReader(p, 1<<20))
	if err != nil {
		return out, err
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		return out, fmt.Errorf("%s ended with %d: %s", cmd[0], code, clipStr(strings.TrimSpace(stderr.String()), 300))
	}
	return out, nil
}

// limited writes at most n bytes to w and drops the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(b []byte) (int, error) {
	if l.n > 0 {
		k := min(len(b), l.n)
		_, _ = l.w.Write(b[:k])
		l.n -= k
	}
	return len(b), nil
}
