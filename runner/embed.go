// Package runner carries the sources of the Agent SDK runner (the Node program that runs in each project's
// VM) inside the Rubi Home binary, so the VM image can be built without a checkout of this repository. The
// image's tag is derived from Hash: a Rubi Home with a changed runner builds (and uses) a new image.
package runner

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"io/fs"
	"sort"
	"sync"
)

// Not embedded: node_modules, dist and test (they are build output or development-only).
//
//go:embed package.json package-lock.json tsconfig.json tsconfig.build.json src container
var files embed.FS

// FS is the runner's build context: package.json, package-lock.json, tsconfig*.json, src/ and container/.
func FS() fs.FS { return files }

var (
	hashOnce sync.Once
	hashHex  string
)

// Hash is the hex SHA-256 over the sorted entries of FS: each file's path and contents, and each directory
// and other kind of entry (a symlink, say) by path and kind, all length-prefixed, so no two different trees
// give the same input and a planted extra entry changes the hash.
func Hash() string {
	hashOnce.Do(func() { hashHex = HashFS(files) })
	return hashHex
}

// HashFS computes Hash for any file system (it never follows a symlink).
func HashFS(fsys fs.FS) string {
	type entry struct {
		path string
		kind byte
	}
	var entries []entry
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return nil
		}
		switch t := d.Type(); {
		case t.IsRegular():
			entries = append(entries, entry{p, 'f'})
		case t.IsDir():
			entries = append(entries, entry{p, 'd'})
		default:
			entries = append(entries, entry{p, 'o'})
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	put := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, e := range entries {
		var content []byte
		if e.kind == 'f' {
			b, err := fs.ReadFile(fsys, e.path)
			if err != nil {
				e.kind = 'x' // unreadable: counts as something else
			}
			content = b
		}
		put([]byte{e.kind})
		put([]byte(e.path))
		put(content)
	}
	return hex.EncodeToString(h.Sum(nil))
}
