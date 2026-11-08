package agentbridge

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"syscall"
)

func configureChild(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
func killChild(c *exec.Cmd) {
	if c.Process != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// Node/libuv can leave inherited standard pipes O_NONBLOCK. These Go stdio
// files are not registered with the poller; a full pipe would otherwise drop a
// frame on EAGAIN. The relay owns these descriptors, not a network bypass.
func blockingStdio(in io.ReadCloser, out io.WriteCloser) error {
	for _, v := range []any{in, out} {
		if f, ok := v.(*os.File); ok {
			if err := syscall.SetNonblock(int(f.Fd()), false); err != nil {
				return errors.New("bridge: blocking control pipes required")
			}
		}
	}
	return nil
}

// Inherited stdio can be nonblocking even after initialization (libuv owns the
// shared open-file description). Retry EAGAIN without dropping partial frames.
type pollFile struct {
	*os.File
	fd int32
}

func (f *pollFile) wait(events int16) error {
	_, err := unix.Poll([]unix.PollFd{{Fd: f.fd, Events: events}}, 1000)
	return err
}
func (f *pollFile) Read(p []byte) (int, error) {
	for {
		n, err := f.File.Read(p)
		if !errors.Is(err, syscall.EAGAIN) {
			return n, err
		}
		if err = f.wait(unix.POLLIN); err != nil && !errors.Is(err, syscall.EINTR) {
			return 0, err
		}
	}
}
func (f *pollFile) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := f.File.Write(p)
		total += n
		p = p[n:]
		if errors.Is(err, syscall.EAGAIN) {
			if e := f.wait(unix.POLLOUT); e != nil && !errors.Is(e, syscall.EINTR) {
				return total, e
			}
			continue
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
func wrapStdio(in io.ReadCloser, out io.WriteCloser) (io.ReadCloser, io.WriteCloser) {
	if f, ok := in.(*os.File); ok {
		in = &pollFile{f, int32(f.Fd())}
	}
	if f, ok := out.(*os.File); ok {
		out = &pollFile{f, int32(f.Fd())}
	}
	return in, out
}
