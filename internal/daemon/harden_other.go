//go:build !linux

package daemon

// harden is Linux-only (PR_SET_DUMPABLE); on macOS, development machines run Rubi as is.
func harden() {}
