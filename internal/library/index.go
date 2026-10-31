package library

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// A sealed entry file cannot be forged, but an older copy of it can be put back (a backup restored over one file, a
// withdrawn version made live again). Every save of an entry raises its Counter, and a sealed index file keeps the
// highest Counter seen per entry; an entry file with a lower one is refused. This does not stop someone who restores
// the whole directory, index included.

const indexFile = ".index.enc" // not a valid entry id, so no skill can have this name

type idxRec struct {
	Counter uint64 `json:"c"`
	Next    int    `json:"n,omitempty"` // highest version number given, kept after the entry is removed
}

type counters struct {
	loaded bool
	m      map[string]idxRec
}

func (l *Library) indexPath() string { return filepath.Join(l.opt.Dir, indexFile) }

func (l *Library) indexAD() []byte { return []byte("rubi-library-index-v1|" + l.opt.Instance) }

// index returns the counters, reading them on first use. A missing or damaged index starts empty (and is logged
// when damaged): the entry files remain the truth.
func (l *Library) index() (*counters, error) {
	if l.idx != nil && l.idx.loaded {
		return l.idx, nil
	}
	a, err := l.aead()
	if err != nil {
		return nil, err
	}
	c := &counters{loaded: true, m: map[string]idxRec{}}
	b, err := os.ReadFile(l.indexPath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case len(b) < a.NonceSize():
		l.logf("library: the index file is damaged; counters start again")
	default:
		plain, err := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], l.indexAD())
		var f struct {
			IDs map[string]idxRec `json:"ids"`
		}
		if err != nil || json.Unmarshal(plain, &f) != nil {
			l.logf("library: the index file is damaged or sealed under another key; counters start again")
		} else if f.IDs != nil {
			c.m = f.IDs
		}
	}
	l.idx = c
	return c, nil
}

func (l *Library) saveIndex(c *counters) error {
	a, err := l.aead()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(map[string]any{"ids": c.m})
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	if err := os.MkdirAll(l.opt.Dir, 0o700); err != nil {
		return err
	}
	return vault.WriteFileAtomic(l.indexPath(), append(nonce, a.Seal(nil, nonce, plain, l.indexAD())...), 0o600)
}

// checkCounter refuses an entry file older than the highest save seen.
func (l *Library) checkCounter(e *Entry) error {
	c, err := l.index()
	if err != nil {
		return err
	}
	if rec := c.m[e.ID]; e.Counter < rec.Counter {
		return fmt.Errorf("library entry %s is an older copy than the one Rubi saved last; it was refused", e.ID)
	}
	return nil
}

// remove deletes an entry file; the index keeps its counter (raised) and version numbers.
func (l *Library) remove(e *Entry) error {
	c, err := l.index()
	if err != nil {
		return err
	}
	rec := c.m[e.ID]
	rec.Counter = max(rec.Counter, e.Counter) + 1
	rec.Next = max(rec.Next, e.nextVersion()-1)
	c.m[e.ID] = rec
	if err := l.saveIndex(c); err != nil {
		return err
	}
	if err := os.Remove(l.path(e.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
