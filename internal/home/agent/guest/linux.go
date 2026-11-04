//go:build linux

package guest

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// InitMounts makes what the guest needs before anything runs: /proc, /sys, /dev and the writable tmpfs places of a
// read-only root (/tmp for everyone, /run for the agent).
func InitMounts() error {
	for _, m := range []struct {
		src, target, fstype string
		flags               uintptr
		data                string
	}{
		{"proc", "/proc", "proc", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, ""},
		{"sysfs", "/sys", "sysfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_RDONLY, ""},
		{"devtmpfs", "/dev", "devtmpfs", unix.MS_NOSUID | unix.MS_NOEXEC, "mode=0755"},
		{"tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV, "mode=1777"},
		{"tmpfs", "/run", "tmpfs", unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC, "mode=0755"},
	} {
		if err := unix.Mount(m.src, m.target, m.fstype, m.flags, m.data); err != nil && err != unix.EBUSY {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	return nil
}

// MountShare mounts one virtio-fs share; never with set-user-id programs or device files.
func MountShare(m Mount) error {
	if err := os.MkdirAll(m.Target, 0o755); err != nil {
		return err
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	return unix.Mount(m.Tag, m.Target, "virtiofs", flags, "")
}

// LinuxServer is the guest agent's server with the real system under it.
func LinuxServer() *Server {
	return &Server{
		Mount: MountShare,
		PowerOff: func() error {
			unix.Sync()
			return unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		},
		// The process runs as the request's user with no extra groups, in a session (and process group) of its own.
		Prepare: func(cmd *exec.Cmd, req ExecRequest) {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: uint32(req.UID), Gid: uint32(req.GID), Groups: []uint32{}},
				Setsid:     true,
				Pdeathsig:  syscall.SIGKILL,
			}
		},
		// The whole group: what the process started goes with it.
		Kill: func(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) },
		Logf: log.Printf,
	}
}

// Reap waits for every child of PID 1 until child (the server) ends, and returns its status. As PID 1 the guest agent
// inherits every orphan of the VM; the server runs as a child of its own, so this wait for any child never takes a
// status the server's exec.Cmd waits for.
func Reap(child int) int {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return -1
		}
		if pid == child {
			return ws.ExitStatus()
		}
	}
}
