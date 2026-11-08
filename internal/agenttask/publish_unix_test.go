//go:build linux || darwin

package agenttask

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestTaskConfigFIFORefusesWithoutBlockingSupervisor(t *testing.T) {
	st, w := taskFixture(t)
	path := filepath.Join(w.Dir, ".git", "config")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Resolve(st.Home, w.ID, w.Project, w.Role, w.SourceHash); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO configuration accepted")
		}
	case <-time.After(time.Second):
		f, _ := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if f != nil {
			f.Close()
		}
		t.Fatal("mutable FIFO blocked trusted supervisor")
	}
}
func TestTaskJournalPlatformRefusalIsBeforeStorage(t *testing.T) {
	for _, goos := range []string{"windows", "freebsd"} {
		if err := CheckPlatform(goos); err == nil {
			t.Fatal("unsupported task platform admitted")
		}
	}
}
