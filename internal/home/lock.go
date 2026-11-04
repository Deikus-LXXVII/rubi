package home

import (
	"os"

	"github.com/Deikus-LXXVII/rubi/internal/home/fsx"
)

// lockFile takes an exclusive advisory lock on path, creating it; the returned function releases it. The lock
// belongs to the open file, so it also orders two Helpers of one process.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := fsx.Lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		fsx.Unlock(f)
		f.Close()
	}, nil
}
