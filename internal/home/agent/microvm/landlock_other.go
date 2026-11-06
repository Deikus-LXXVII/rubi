//go:build !linux

package microvm

// LandlockABI: Landlock is Linux's alone.
func LandlockABI() (int, error) { return 0, ErrNoLandlock }

// ExecConfined: nothing runs unconfined.
func ExecConfined(rules []FSRule, argv, env []string) error { return ErrNoLandlock }
