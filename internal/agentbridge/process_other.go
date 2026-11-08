//go:build !linux

package agentbridge

import (
	"errors"
	"io"
	"os/exec"
)

func configureChild(c *exec.Cmd) {}
func killChild(c *exec.Cmd) {
	if c.Process != nil {
		c.Process.Kill()
	}
}

func blockingStdio(in io.ReadCloser, out io.WriteCloser) error {
	return errors.New("bridge: Linux required")
}

func wrapStdio(in io.ReadCloser, out io.WriteCloser) (io.ReadCloser, io.WriteCloser) { return in, out }
