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

// Hash is the hex SHA-256 over the sorted paths and contents of FS (each length-prefixed, so no two
// different bundles give the same input).
func Hash() string {
	hashOnce.Do(func() { hashHex = HashFS(files) })
	return hashHex
}

// HashFS computes Hash for any file system.
func HashFS(fsys fs.FS) string {
	var paths []string
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	h := sha256.New()
	put := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			continue
		}
		put([]byte(p))
		put(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}
