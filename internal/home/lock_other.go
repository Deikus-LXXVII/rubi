//go:build !(darwin || linux || freebsd || netbsd || openbsd)

package home

// lockFile does nothing where there is no flock; the in-process mutex still orders one helper's writes.
func lockFile(string) (func(), error) { return func() {}, nil }
