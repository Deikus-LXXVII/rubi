//go:build !darwin && !linux

package home

import "errors"

func procExe(int) (string, error) { return "", errors.New("process lookup isn't supported on this OS") }
