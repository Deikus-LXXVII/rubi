// Package fsx holds the few file-system and process calls of Rubi Home whose Unix and Windows forms differ: an
// advisory file lock, opening without following a link, who owns a file, how many hard links it has, and
// killing a process with its children. Each has one signature and a file per system (fsx_unix.go,
// fsx_windows.go), so the code that calls it is the same everywhere (docs/plans/home-platforms.md, stage P1).
//
// Where Windows has no equal for a Unix answer, the helper says so ("unknown") and the caller must treat that as
// the unsafe answer: for example OwnerOf reports no owner on Windows, so nothing there counts as root-owned until
// stage P2 gives it a real check.
package fsx

import "errors"

// ErrLocked is what TryLock returns when another holder has the lock.
var ErrLocked = errors.New("the file is locked by another process")

// ErrLink is what OpenNoFollow returns on Windows when the last element of the path is a link.
var ErrLink = errors.New("the path is a link; not following it")
