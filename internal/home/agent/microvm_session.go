//go:build unix

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
	"github.com/Deikus-LXXVII/rubi/internal/hostguard"
)

// SessionVMMemory is a T3 session VM's memory (MiB): one provider CLI and its tools, on a computer that may have only a
// few GiB free (docs/plans/t3-cloud-agents.md slice 3).
const SessionVMMemory = 1024

// sessionMemoryMargin is what must stay free beside a new session VM.
const sessionMemoryMargin = 256

// StreamSessions implements agent.StreamSessions: T3 sessions run in microVMs Home starts itself.
func (b *MicroVMBackend) StreamSessions() StreamSessionHost { return microSessions{b: b} }

type microSessions struct{ b *MicroVMBackend }

// sessionWarmup is how long a new session VM's memory counts as taken although the computer doesn't show it yet (a
// guest touches its memory as it runs).
const sessionWarmup = time.Minute

func (s microSessions) Ready() error {
	s.b.sessMu.Lock()
	defer s.b.sessMu.Unlock()
	return s.readyLocked()
}

// readyLocked is Ready with sessMu held: the free memory must hold a new VM besides those started within the warm-up.
func (s microSessions) readyLocked() error {
	if err := s.b.ready(); err != nil {
		return err
	}
	now := time.Now
	if s.b.now != nil {
		now = s.b.now
	}
	recent := s.b.recentSessions[:0]
	for _, t := range s.b.recentSessions {
		if now().Sub(t) < sessionWarmup {
			recent = append(recent, t)
		}
	}
	s.b.recentSessions = recent
	avail := s.b.memAvailable
	if avail == nil {
		avail = hostguard.MemAvailable
	}
	need := (len(recent)+1)*SessionVMMemory + sessionMemoryMargin
	if mib, ok := avail(); ok && mib < need {
		return fmt.Errorf("only %d MiB of memory are free; a session VM needs %d MiB", mib, need-len(recent)*SessionVMMemory)
	}
	return nil
}

func (s microSessions) StartSessionVM(ctx context.Context, name string, mounts []Mount) (SessionVM, error) {
	s.b.sessMu.Lock()
	if err := s.readyLocked(); err != nil {
		s.b.sessMu.Unlock()
		return nil, err
	}
	now := time.Now
	if s.b.now != nil {
		now = s.b.now
	}
	s.b.recentSessions = append(s.b.recentSessions, now()) // reserved before the boot, so starts at once can't all pass
	s.b.sessMu.Unlock()
	return startSessionVM(ctx, s.b.New(Project{}).(*MicroVM), name, mounts)
}

// startSessionVM boots m as a session VM named name with exactly mounts shared.
func startSessionVM(ctx context.Context, m *MicroVM, name string, mounts []Mount) (SessionVM, error) {
	m.name, m.Memory = name, fmt.Sprint(SessionVMMemory)
	m.shares = make([]microvm.Share, 0, len(mounts))
	for _, mt := range mounts {
		if badMountPath(mt.Source) || badMountPath(mt.Target) {
			return nil, errors.New("a session folder can't be mounted (a comma or control character in its path)")
		}
		m.shares = append(m.shares, microvm.Share{Source: mt.Source, Target: mt.Target, ReadOnly: mt.ReadOnly})
	}
	m.setState(StateStarting)
	if err := m.boot(ctx, Project{}); err != nil {
		m.setState(StateFailed)
		return nil, err
	}
	m.setState(StateRunning)
	return microSession{m}, nil
}

type microSession struct{ m *MicroVM }

func (v microSession) StartPipe(ctx context.Context, spec *PipeSpec, env map[string]string, giveUp func()) (func(), error) {
	p, err := startPipeWith(ctx, v.m.name, v.m.exec, spec, env, giveUp)
	if err != nil {
		return nil, err
	}
	return p.stop, nil
}

func (v microSession) ExecIn(ctx context.Context, dir string, env map[string]string, argv []string, stderr io.Writer) (SessionProc, error) {
	p, err := v.m.execIn(ctx, dir, env, argv, stderr)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (v microSession) Stop(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*microVMPowerOff+5*time.Second)
	defer cancel()
	return v.m.Stop(ctx)
}
