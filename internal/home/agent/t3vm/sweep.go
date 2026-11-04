package t3vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// ContainerPrefix starts the name of every container this package makes (sessions and version probes).
const ContainerPrefix = "rubi-t3-"

// lockAndSweep takes the lock that says "this Home serves T3 sessions on this directory" and removes what an earlier
// run left: every rubi-t3-* container and the environment files. The lock is held until Serve returns, so a second
// Home on the same directory neither serves nor removes the live VMs of the first; it is refused instead.
//
// A VM that survives a crash of Home is fail-closed anyway: its hook gate is gone (an unreachable gate is a deny),
// and so is its proxy (no network). Nothing in the VM notices that Home is gone and exits by itself: the forwarder
// does (its stdin closes), but the container's own process is `sleep infinity`. The sweep at the next start, or the
// connection closing for a session whose shim died, is what removes it.
func (s *Server) lockAndSweep(ctx context.Context) error {
	dir := filepath.Join(s.opt.Dir, "t3vm")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := fsx.OpenNoFollow(filepath.Join(dir, "sessions.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := fsx.TryLock(f); err != nil {
		f.Close()
		return fmt.Errorf("another Rubi Home already serves T3 sessions on %s", s.opt.Dir)
	}
	s.lock = f
	sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	names, err := s.opt.Host.ListContainers(sctx)
	if err != nil {
		s.opt.Logf("t3vm: not sweeping old VMs: %v", err)
	}
	for _, n := range names {
		if !strings.HasPrefix(n, ContainerPrefix) {
			continue
		}
		if err := s.opt.Host.RemoveContainer(sctx, n); err != nil {
			s.opt.Logf("t3vm: removing the old VM %s: %v", n, err)
		}
	}
	env := filepath.Join(dir, "env")
	if ents, err := os.ReadDir(env); err == nil {
		for _, e := range ents {
			_ = os.RemoveAll(filepath.Join(env, e.Name()))
		}
	}
	return nil
}

func (s *Server) unlock() {
	if s.lock != nil {
		s.lock.Close()
		s.lock = nil
	}
}
