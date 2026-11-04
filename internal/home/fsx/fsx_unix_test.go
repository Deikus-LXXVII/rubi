//go:build unix

package fsx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestOwnerOfIsTheFilesUser(t *testing.T) {
	p := t.TempDir() + "/f"
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	uid, ino, ok := OwnerOf(fi)
	if !ok || int(uid) != os.Getuid() || ino == 0 {
		t.Fatalf("got uid %d inode %d ok %v; this user is %d", uid, ino, ok, os.Getuid())
	}
}

// A child that leaves a grandchild holding the output pipe keeps Wait waiting; KillTree ends both at once.
func TestKillTreeEndsTheChildrenToo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 20 & wait")
	var out bytes.Buffer
	cmd.Stdout = &out
	KillTree(cmd)
	start := time.Now()
	_ = cmd.Run()
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the grandchild outlived the cancel: Run took %s", d)
	}
}
