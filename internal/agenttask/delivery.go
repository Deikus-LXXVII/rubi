package agenttask

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// Delivery receipts live next to the journal, not in a growing plugin-state map.
// They do not mutate terminal task state. Emit followed by Ack is deliberately
// at-least-once: a receiver deduplicates EventID across the emit/ack crash window.
func (s Store) ResultAcknowledged(ctx context.Context, id string) bool {
	root, err := s.root()
	if err != nil || !ValidID(id) {
		return false
	}
	f, err := fsx.OpenNoFollow(filepath.Join(root, id+".delivered"), os.O_RDONLY|fsx.NonBlock, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 128 {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, 129))
	return ctx.Err() == nil && err == nil && string(b) == id+":terminal"
}
func (s Store) AcknowledgeResult(ctx context.Context, id, event string) error {
	state, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !state.Phase.Terminal() || event != state.EventID() {
		return errors.New("task: acknowledgement requires the exact completed-task event")
	}
	if err = s.Quiesce(ctx, id); err != nil {
		return err
	}
	root, err := s.root()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".delivery-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(event); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(root, id+".delivered")); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
