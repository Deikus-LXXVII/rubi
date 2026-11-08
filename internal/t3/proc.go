package t3

// Reading another process: its parent and its argument vector, from a /proc tree (Linux) or from the
// kern.procargs2 sysctl (macOS). The parsers are portable so the tests run everywhere.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procStat is what Rubi reads from /proc/<pid>/stat: the parent, the process group and the start time (clock
// ticks since boot), which tells one process from a later one that got the same id.
type procStat struct {
	PPID, PGID int
	Start      uint64
}

// statIn reads /proc/<pid>/stat under root.
func statIn(root string, pid int) (procStat, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	return parseStat(string(b))
}

// parseStat parses "pid (comm) state ppid pgrp session tty tpgid flags ... starttime ...". The command name may
// hold spaces and parentheses, so the fields are cut after the last ')'.
func parseStat(s string) (procStat, error) {
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, errors.New("t3: unreadable process stat")
	}
	f := strings.Fields(s[i+1:]) // f[0] is field 3 (state), so field n is f[n-3]
	if len(f) < 20 {
		return procStat{}, errors.New("t3: short process stat")
	}
	ppid, err1 := strconv.Atoi(f[1])
	pgid, err2 := strconv.Atoi(f[2])
	start, err3 := strconv.ParseUint(f[19], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return procStat{}, errors.New("t3: unreadable process stat")
	}
	return procStat{PPID: ppid, PGID: pgid, Start: start}, nil
}

// argvIn reads /proc/<pid>/cmdline under root.
func argvIn(root string, pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, errors.New("t3: no command line") // a zombie or a kernel thread
	}
	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), nil
}

// parseProcArgs2 parses macOS's kern.procargs2: argc (a 32-bit native-endian int, little-endian on every Mac
// Rubi runs on), the executable path, NUL padding, then argc NUL-terminated arguments (the environment follows).
func parseProcArgs2(b []byte) ([]string, error) {
	if len(b) < 4 {
		return nil, errors.New("t3: short procargs")
	}
	argc := int(binary.LittleEndian.Uint32(b))
	rest := b[4:]
	i := bytes.IndexByte(rest, 0) // end of the executable path
	if argc <= 0 || argc > 4096 || i < 0 {
		return nil, errors.New("t3: unreadable procargs")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	args := make([]string, 0, argc)
	for range argc {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			return nil, errors.New("t3: truncated procargs")
		}
		args = append(args, string(rest[:j]))
		rest = rest[j+1:]
	}
	return args, nil
}
