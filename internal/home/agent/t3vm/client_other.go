//go:build !unix

package t3vm

import (
	"errors"
	"os"
)

func execProcess(string, []string, []string, *os.File) error {
	return errors.New("this system can't replace the process")
}
