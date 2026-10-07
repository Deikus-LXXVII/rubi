package daemon

import (
	"log"

	"golang.org/x/sys/unix"
)

// harden marks the daemon non-dumpable, so other processes of the same user (plugins included) can't
// read its memory through ptrace or /proc/<pid>/mem while it holds the vault key.
func harden() {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		log.Printf("couldn't mark the daemon non-dumpable: %v", err)
	}
}
