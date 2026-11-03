package t3vm

// Self-test support (t3-setup.run selftest): the same VM a session gets, started for Home's own use. Probe starts it
// through the session path (the same role checks, caps, mounts, proxy and hook gate), runs a command in it instead of
// claude and removes it; Version asks the image for claude's version as the shim's first call does.

import (
	"context"
	"errors"
	"slices"
	"time"
)

// probeLimit bounds a probe VM's whole life.
const probeLimit = 5 * time.Minute

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
