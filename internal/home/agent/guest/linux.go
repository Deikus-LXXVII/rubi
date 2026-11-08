//go:build linux

package guest

import (
	"errors"
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

// LoopbackUp brings up the VM's loopback interface, its only one. The kernel leaves it down; a container runtime brings
// it up, the guest's init has to. Up, it gets 127.0.0.1 and ::1, where the runner's forwarder (rubi-netpipe) listens
// for the proxy and the other services Home offers through the pipe.
func LoopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("the loopback interface: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("the loopback interface: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("the loopback interface: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("bringing up the loopback interface: %w", err)
	}
	return nil
}

// MountShare mounts one virtio-fs share; never with set-user-id programs or device files.
func MountShare(m Mount) error {
	if err := os.MkdirAll(m.Target, 0o755); err != nil {
		if errors.Is(err, unix.EROFS) {
			return fmt.Errorf("the mount point %s can't be made: the root is read-only (the writable layer over it didn't come up)", m.Target)
		}
		return err
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if m.ReadOnly {
		flags |= unix.MS_RDONLY
	}
	return unix.Mount(m.Tag, m.Target, "virtiofs", flags, "")
}

// LinuxServer is the guest agent's server with the real system under it. PowerOff restarts rather than powers off: with
// QEMU's -no-reboot a restart ends QEMU on every machine (x86's microvm has no ACPI power button; reboot=t on its
// command line makes the restart a triple fault), and a halted VM that never exits can't happen.
func LinuxServer() *Server {
	return &Server{
		Mount: MountShare,
		PowerOff: func() error {
			unix.Sync()
			return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
		},
		// The process runs as the request's user with no extra groups, in a session (and process group) of its own.
		Prepare: func(cmd *exec.Cmd, req ExecRequest) {
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Credential: &syscall.Credential{Uid: uint32(req.UID), Gid: uint32(req.GID), Groups: []uint32{}},
				Setsid:     true,
			}
		},
		Kill:      killGroup,
		AwaitExit: awaitExit,
		Logf:      log.Printf,
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

// killGroup ends the command's process group: what the process started goes with it.
func killGroup(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

// awaitExit waits until the command's leader has exited, leaving it unreaped (its pid, and so its group's id, stay
// taken until exec.Cmd.Wait).
func awaitExit(cmd *exec.Cmd) {
	var info unix.Siginfo
	for unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil) == unix.EINTR {
	}
}

// OverlayDir is where OverlayRoot builds the layer (a folder of the root file system, made when the image is built).
const OverlayDir = "/rubi-overlay"

// OverlaySize bounds what can be written over the root: a tmpfs, in the VM's own memory.
const OverlaySize = "256m"

// OverlayRoot puts a writable layer over the read-only root, before anything else is mounted: an overlay of the root
// with a size-bounded tmpfs on top, made the new root. Mount points can then be made anywhere (a T3 session's shares
// are at the host's own paths), and nothing is written to the root file system, which the host attaches read-only.
// It runs as PID 1 only, with no other process.
func OverlayRoot() error {
	if err := unix.Mount("tmpfs", OverlayDir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700,size="+OverlaySize); err != nil {
		return fmt.Errorf("the overlay's tmpfs: %w", err)
	}
	upper, work, merged := OverlayDir+"/upper", OverlayDir+"/work", OverlayDir+"/root"
	for _, d := range []string{upper, work, merged} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	// lowerdir=/ is the root file system's own tree: mounts on top of it (this tmpfs) are not part of it
	if err := unix.Mount("overlay", merged, "overlay", 0, "lowerdir=/,upperdir="+upper+",workdir="+work); err != nil {
		return fmt.Errorf("the overlay: %w", err)
	}
	if err := os.Chdir(merged); err != nil {
		return err
	}
	const old = "rubi-old-root"
	if err := os.Mkdir(old, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", old); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return err
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	// the old root (and the tmpfs on it) stay reachable to the overlay only
	if err := unix.Unmount("/"+old, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detaching the old root: %w", err)
	}
	return os.Remove("/" + old)
}
