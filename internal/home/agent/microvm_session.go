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

func (s microSessions) Ready() error {
	if err := s.b.ready(); err != nil {
		return err
	}
	avail := s.b.memAvailable
	if avail == nil {
		avail = hostguard.MemAvailable
	}
	if mib, ok := avail(); ok && mib < SessionVMMemory+sessionMemoryMargin {
		return fmt.Errorf("only %d MiB of memory are free; a session VM needs %d MiB", mib, SessionVMMemory+sessionMemoryMargin)
	}
	return nil
}

func (s microSessions) StartSessionVM(ctx context.Context, name string, mounts []Mount) (SessionVM, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
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
