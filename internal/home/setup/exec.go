package setup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Commander runs commands on the Mac. It is the only way this package starts a process, so tests give it a
// fake and never run a real command. argv[0] == "container" means the container CLI wherever it is installed.
type Commander interface {
	// Run runs argv to completion and returns its combined output (at most 1 MiB).
	Run(ctx context.Context, argv []string) ([]byte, error)
	// Stream runs argv, giving each line of its combined output (split at newlines and carriage returns) to
	// line, and returns when it ends.
	Stream(ctx context.Context, argv []string, line func(string)) error
}

type osCommander struct{}

func resolve(argv []string) *exec.Cmd {
	bin := argv[0]
	if bin == "container" {
		bin = agent.ContainerBin()
	}
	return exec.Command(bin, argv[1:]...)
}

type capped struct {
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := 1<<20 - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (osCommander) Run(ctx context.Context, argv []string) ([]byte, error) {
	cmd := resolve(argv)
	var out capped
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	err := cmd.Wait()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", strings.Join(argv[:min(len(argv), 3)], " "), err)
	}
	return out.buf.Bytes(), err
}

func (osCommander) Stream(ctx context.Context, argv []string, line func(string)) error {
	cmd := resolve(argv)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		waited <- err
	}()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	sc.Split(splitLines)
	for sc.Scan() {
		line(sc.Text())
	}
	_, _ = io.Copy(io.Discard, pr)
	err := <-waited
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", strings.Join(argv[:min(len(argv), 3)], " "), err)
	}
	return err
}

// splitLines splits at \n and \r (build tools redraw a progress line with \r).
func splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
