package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockExcludesAnotherHolder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	a, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := TryLock(a); err != nil {
		t.Fatalf("first TryLock: %v", err)
	}
	if err := TryLock(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock while held: got %v, want ErrLocked", err)
	}
	Unlock(a)
	if err := TryLock(b); err != nil {
		t.Fatalf("TryLock after Unlock: %v", err)
	}
	Unlock(b)
	if err := Lock(a); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	Unlock(a)
}

func TestOpenNoFollowRefusesALinkAndOpensAFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenNoFollow(real, os.O_RDONLY|NonBlock, 0)
	if err != nil {
		t.Fatalf("a plain file: %v", err)
	}
	f.Close()

	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	if f, err := OpenNoFollow(link, os.O_RDONLY, 0); err == nil {
		f.Close()
		t.Fatal("a link was followed")
	}
	// creating a file that is not there works, and O_EXCL still refuses one that is
	fresh := filepath.Join(dir, "fresh")
	if f, err := OpenNoFollow(fresh, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		t.Fatalf("create: %v", err)
	} else {
		f.Close()
	}
	if f, err := OpenNoFollow(fresh, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		f.Close()
		t.Fatal("O_EXCL opened an existing file")
	}
	// a link to nothing is still a link
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err == nil {
		if f, err := OpenNoFollow(dangling, os.O_WRONLY|os.O_CREATE, 0o600); err == nil {
			f.Close()
			t.Fatal("a dangling link was followed")
		}
	}
}

func TestLinkCount(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() uint64 {
		f, err := os.Open(a)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		n, ok := LinkCount(f)
		if !ok {
			t.Fatal("link count unknown")
		}
		return n
	}
	if n := open(); n != 1 {
		t.Fatalf("one name: got %d links", n)
	}
	if err := os.Link(a, filepath.Join(dir, "b")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if n := open(); n != 2 {
		t.Fatalf("two names: got %d links", n)
	}
}
