package fsx

import (
	"os"
	"testing"
)

// There is no owner on Windows: every caller must then treat the file as not an administrator's.
func TestOwnerOfSaysUnknown(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := OwnerOf(fi); ok {
		t.Fatal("Windows reported an owner")
	}
}
