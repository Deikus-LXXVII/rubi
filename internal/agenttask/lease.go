package agenttask

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// Lease is a trusted launcher receipt, not a native thread or an agent PID.
// Only Close, AFTER the confined process exits, acknowledges quiescence.
// A crash leaves an incomplete receipt: an unlocked file alone proves nothing.
type Lease struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	joined chan struct{}
	once   sync.Once
	file   *os.File
}

func (l *Lease) Context() context.Context { return l.ctx }
func (l *Lease) Close() {
	l.once.Do(func() {
		close(l.done)
		<-l.joined
		l.cancel()
		// Failure to persist completion deliberately leaves cancellation unconfirmed.
		if _, err := l.file.WriteAt([]byte("done\n"), 0); err == nil {
			_ = l.file.Sync()
		}
		fsx.Unlock(l.file)
		_ = l.file.Close()
	})
}

func (s Store) leaseGate(ctx context.Context, id string) (*os.File, string, error) {
	if !ValidID(id) {
		return nil, "", errors.New("task: invalid task ID")
	}
	root, err := s.root()
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(root, id+".leases")
	if err = privateDir(dir); err != nil {
		return nil, "", err
	}
	f, err := fsx.OpenNoFollow(filepath.Join(dir, "gate"), os.O_CREATE|os.O_RDWR|fsx.NonBlock, 0600)
	if err != nil {
		return nil, "", err
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, "", errors.New("task: unsafe launcher gate")
	}
	if err = waitLeaseLock(ctx, f); err != nil {
		f.Close()
		return nil, "", err
	}
	return f, dir, nil
}
func waitLeaseLock(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fsx.TryLock(f); err == nil {
			return nil
		} else if !errors.Is(err, fsx.ErrLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Admit and Quiesce share a separate gate, never the journal lock. Admission is
// safe even when the caller holds a journal transaction during an append/retry.
func (s Store) Admit(ctx context.Context, id string) (*Lease, error) {
	gate, dir, err := s.leaseGate(ctx, id)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	defer fsx.Unlock(gate)
	state, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if state.Spec.Supervision != 1 {
		return nil, errors.New("task: supervised launch attestation required")
	}
	if !state.Phase.Mutable() {
		return nil, errors.New("task: mandate sealed or ended; provider launch refused")
	}
	f, err := os.CreateTemp(dir, "run-")
	if err != nil {
		return nil, err
	}
	if err = fsx.TryLock(f); err != nil {
		f.Close()
		return nil, err
	}
	if _, err = f.Write([]byte("live\n")); err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	d, err := os.Open(dir)
	if err == nil {
		err = d.Sync()
		d.Close()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	l := &Lease{ctx: life, cancel: cancel, done: make(chan struct{}), joined: make(chan struct{}), file: f}
	go func() {
		defer close(l.joined)
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-l.done:
				return
			case <-life.Done():
				return
			case <-tick.C:
				state, e := s.Get(life, id)
				if e != nil || !state.Phase.Mutable() {
					cancel()
					return
				}
			}
		}
	}()
	return l, nil
}

// Quiesce must follow a durable stop/publication fence. It waits for every
// admitted process, including a launch whose T3 reply was lost. Future launches
// see the fence. Never removes receipts: crash evidence must survive restart.
func (s Store) Quiesce(ctx context.Context, id string) error {
	gate, dir, err := s.leaseGate(ctx, id)
	if err != nil {
		return err
	}
	defer gate.Close()
	defer fsx.Unlock(gate)
	state, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if state.Spec.Supervision != 1 {
		return errors.New("task: legacy launch has no supervision receipt; stop its original T3 instance before retiring the task")
	}
	if state.Phase.Mutable() {
		return errors.New("task: quiescence requires a sealed mandate")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "gate" {
			continue
		}
		f, e := fsx.OpenNoFollow(filepath.Join(dir, entry.Name()), os.O_RDONLY|fsx.NonBlock, 0)
		if e != nil {
			return errors.New("task: unsafe launcher receipt")
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			f.Close()
			return errors.New("task: unsafe launcher receipt")
		}
		e = waitLeaseLock(ctx, f)
		if e != nil {
			f.Close()
			return errors.New("task: provider interruption pending; retry")
		}
		b, e := io.ReadAll(io.LimitReader(f, 16))
		fsx.Unlock(f)
		f.Close()
		if e != nil || string(b) != "done\n" {
			return errors.New("task: trusted launcher exited without a cleanup receipt; interruption unconfirmed")
		}
	}
	return nil
}
