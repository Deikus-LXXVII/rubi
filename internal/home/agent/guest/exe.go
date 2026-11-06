package guest

import "os"

// isExecutable says whether p is a regular file someone may run.
func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
