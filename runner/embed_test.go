package runner

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedBundle(t *testing.T) {
	for _, p := range []string{"package.json", "package-lock.json", "tsconfig.build.json", "src/main.ts", "container/Dockerfile", "container/git-credential-rubi", "container/rubi-claude-token"} {
		if _, err := fs.Stat(FS(), p); err != nil {
			t.Errorf("%s isn't embedded: %v", p, err)
		}
	}
	for _, p := range []string{"test", "node_modules", "dist", "testdata"} {
		if _, err := fs.Stat(FS(), p); err == nil {
			t.Errorf("%s must not be embedded", p)
		}
	}
	if len(Hash()) != 64 || Hash() != HashFS(FS()) {
		t.Errorf("hash: %q", Hash())
	}
}

func TestHashDependsOnPathsAndContents(t *testing.T) {
	a := fstest.MapFS{"a": {Data: []byte("x")}, "b": {Data: []byte("y")}}
	if HashFS(a) != HashFS(fstest.MapFS{"b": {Data: []byte("y")}, "a": {Data: []byte("x")}}) {
		t.Error("not stable")
	}
	for name, other := range map[string]fstest.MapFS{
		"content":   {"a": {Data: []byte("x")}, "b": {Data: []byte("z")}},
		"path":      {"a": {Data: []byte("x")}, "c": {Data: []byte("y")}},
		"joined":    {"a": {Data: []byte("xb")}},
		"extra dir": {"a": {Data: []byte("x")}, "b": {Data: []byte("y")}, "d": {Mode: fs.ModeDir}},
		"symlink":   {"a": {Data: []byte("x")}, "b": {Data: []byte("y")}, "l": {Data: []byte("a"), Mode: fs.ModeSymlink}},
	} {
		if HashFS(a) == HashFS(other) {
			t.Errorf("%s change not noticed", name)
		}
	}
	if strings.Count(Hash(), "0") == 64 {
		t.Error("empty")
	}
}
