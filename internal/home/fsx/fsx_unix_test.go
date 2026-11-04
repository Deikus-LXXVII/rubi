//go:build unix

package fsx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
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

func TestTrustedByAdminOnUnix(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/f"
	if err := os.WriteFile(f, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(f)
	ok, why := TrustedByAdmin(f, fi)
	if os.Getuid() == 0 {
		if !ok {
			t.Errorf("a file of root, mode 0755: %q", why)
		}
		_ = os.Chmod(f, 0o775)
		fi, _ = os.Stat(f)
		if ok, why = TrustedByAdmin(f, fi); ok || !strings.Contains(why, "writable by group or others (mode 0775)") {
			t.Errorf("a group-writable file: %v %q", ok, why)
		}
	} else if ok || why != "not owned by root" {
		t.Errorf("a file of this user: %v %q", ok, why)
	}
	// a system folder is root's and nobody else can write it
	if fi, err := os.Stat("/bin"); err == nil {
		if ok, why := TrustedAncestor("/bin", fi); !ok {
			t.Errorf("/bin: %q", why)
		}
	}
	if AdminName != "root" {
		t.Errorf("AdminName %q", AdminName)
	}
}
