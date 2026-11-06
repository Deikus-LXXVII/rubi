package fsx

import (
	"errors"
	"strings"
)

// ErrNoFinalPath is what FinalPath returns where the system can not name the path of an open file the way Windows can
// (Unix: compare the open file with the path by os.SameFile instead).
var ErrNoFinalPath = errors.New("this system does not give the path of an open file")

// TrimExtendedPrefix turns the path GetFinalPathNameByHandle returns (\\?\C:\dir, \\?\UNC\server\share\dir) into the
// ordinary form (C:\dir, \\server\share\dir). Anything else is returned as it is.
func TrimExtendedPrefix(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\UNC\`):
		return `\\` + p[len(`\\?\UNC\`):]
	case strings.HasPrefix(p, `\\?\`) && len(p) >= 6 && p[5] == ':':
		return p[len(`\\?\`):]
	}
	return p
}
