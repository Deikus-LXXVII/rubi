//go:build !linux && !darwin

package t3

import "errors"

var errNoProc = errors.New("t3: reading other processes isn't supported on this system")

func procOf(int) (procStat, error) { return procStat{}, errNoProc }

func argvOf(int) ([]string, error) { return nil, errNoProc }

func groupOf(int) []int { return nil }
