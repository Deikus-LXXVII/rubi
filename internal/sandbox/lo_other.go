//go:build !linux

package sandbox

import "errors"

func LoUp() error { return errors.New("loopback setup is only implemented on Linux") }
