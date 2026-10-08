package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Store is the on-disk plugin directory:
//
//	plugins/index.json          installed plugins and their manifests (plaintext; only used to list tools)
//	plugins/<id>/<version>/     the extracted package
//	plugins/<id>/data/          the plugin's working directory
//
// Nothing here is trusted on its own: before a plugin starts, its tree hash is checked against the
// record in the vault.
type Store struct {
	Root string

	mu    sync.RWMutex
	index map[string]Manifest
}

type indexFile struct {
	Plugins map[string]Manifest `json:"plugins"`
}

func OpenStore(root string) (*Store, error) {
	s := &Store{Root: root, index: map[string]Manifest{}}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f indexFile
	if err := json.Unmarshal(b, &f); err != nil {
		return s, nil // a broken index lists nothing; plugins can be reinstalled
	}
	for id, m := range f.Plugins {
		if Check(&m) == nil && m.ID == id {
			s.index[id] = m
		}
	}
	return s, nil
}

func (s *Store) indexPath() string { return filepath.Join(s.Root, "index.json") }

// Dir is where version of plugin id is extracted.
func (s *Store) Dir(id, version string) string { return filepath.Join(s.Root, id, version) }

// DataDir is the plugin's working directory.
func (s *Store) DataDir(id string) string { return filepath.Join(s.Root, id, "data") }

// Staging is where downloads are unpacked before approval.
func (s *Store) Staging() string { return filepath.Join(s.Root, ".staging") }

// Installed lists installed manifests, sorted by id.
func (s *Store) Installed() []Manifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Manifest, 0, len(s.index))
	for _, m := range s.index {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Store) Get(id string) (Manifest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.index[id]
	return m, ok
}

// Commit moves a verified candidate into place. Other versions are deleted, except keep (the version
// being replaced, kept for rollback; "" keeps none).
func (s *Store) Commit(c *Candidate, keep string) error {
	tree, err := TreeHash(c.Dir)
	if err != nil {
		return err
	}
	if tree != c.Tree {
		return errors.New("the downloaded plugin changed on disk before installation; try again")
	}
	id, ver := c.Manifest.ID, c.Manifest.Version
	if err := os.MkdirAll(filepath.Join(s.Root, id), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(s.DataDir(id), 0o700); err != nil {
		return err
	}
	dst := s.Dir(id, ver)
	_ = os.RemoveAll(dst)
	if err := os.Rename(c.Dir, dst); err != nil {
		return err
	}
	s.mu.Lock()
	s.index[id] = c.Manifest
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.prune(id, ver, keep)
	return nil
}

// prune deletes every version directory of id except the ones named.
func (s *Store) prune(id string, keep ...string) {
	entries, _ := os.ReadDir(filepath.Join(s.Root, id))
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "data" || slices.Contains(keep, e.Name()) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(s.Root, id, e.Name()))
	}
}

// Activate switches the installed version of id to another version already on disk (a rollback). The
// caller verifies its files first.
func (s *Store) Activate(id, version string) error {
	b, err := os.ReadFile(filepath.Join(s.Dir(id, version), "rubi-plugin.json"))
	if err != nil {
		return fmt.Errorf("version %s of %s isn't on disk any more", version, id)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if err := Check(&m); err != nil {
		return err
	}
	if m.ID != id || m.Version != version {
		return errors.New("the stored manifest doesn't match the version")
	}
	s.mu.Lock()
	s.index[id] = m
	err = s.saveLocked()
	s.mu.Unlock()
	return err
}

// Remove deletes a plugin's files, including its working directory.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	delete(s.index, id)
	err := s.saveLocked()
	s.mu.Unlock()
	if !idPattern.MatchString(id) {
		return errors.New("invalid plugin id")
	}
	if rmErr := os.RemoveAll(filepath.Join(s.Root, id)); rmErr != nil {
		return rmErr
	}
	return err
}

// CleanStaging removes leftover downloads.
func (s *Store) CleanStaging(keep map[string]bool) {
	entries, _ := os.ReadDir(s.Staging())
	for _, e := range entries {
		p := filepath.Join(s.Staging(), e.Name())
		if !keep[p] {
			_ = os.RemoveAll(p)
		}
	}
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(indexFile{Plugins: s.index}, "", "  ")
	if err != nil {
		return err
	}
	return vault.WriteFileAtomic(s.indexPath(), b, 0o600)
}

// Verify checks an installed plugin against the tree hash recorded in the vault.
// Trust re-reads an installed plugin's manifest from its verified files and makes it the one in use,
// replacing what index.json said. The index is plain text anyone on the machine can edit; the manifest
// inside the package is covered by the tree hash the user approved (kept in the vault), so permissions,
// approval levels and the entry point are always taken from there.
func (s *Store) Trust(id, version, tree string) (Manifest, error) {
	if err := s.Verify(id, version, tree); err != nil {
		return Manifest{}, err
	}
	b, err := os.ReadFile(filepath.Join(s.Dir(id, version), "rubi-plugin.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin %s has no manifest: %w", id, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("plugin %s has a broken manifest", id)
	}
	if err := Check(&m); err != nil {
		return Manifest{}, err
	}
	if m.ID != id || m.Version != version {
		return Manifest{}, fmt.Errorf("plugin %s: the manifest doesn't match the installed version", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.index[id]; !ok || !sameManifest(prev, m) {
		s.index[id] = m
		_ = s.saveLocked()
	}
	return m, nil
}

// Retain drops index entries for plugins the user never approved installing.
func (s *Store) Retain(ids map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id := range s.index {
		if !ids[id] {
			delete(s.index, id)
			changed = true
		}
	}
	if changed {
		_ = s.saveLocked()
	}
}

func sameManifest(a, b Manifest) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func (s *Store) Verify(id, version, tree string) error {
	got, err := TreeHash(s.Dir(id, version))
	if err != nil {
		return err
	}
	if got != tree {
		return fmt.Errorf("the files of plugin %s were modified after installation", id)
	}
	return nil
}

// TreeHash hashes a directory: every regular file's relative path, executable bit and content. Links
// and other special files are refused.
func TreeHash(root string) (string, error) {
	type entry struct{ path, line string }
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected special file %s", p)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		exec := "-"
		if info.Mode()&0o111 != 0 {
			exec = "x"
		}
		entries = append(entries, entry{rel, rel + "\x00" + exec + "\x00" + hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.line)
		b.WriteByte('\n')
	}
	return sha256Hex([]byte(b.String())), nil
}
