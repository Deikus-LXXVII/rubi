// Package microvm builds what the Linux VM backend of Rubi Home runs (docs/plans/home-platforms.md, stage P9, and
// docs/design/linux-microvm.md): the command line of one QEMU/KVM process per VM, the command lines of the virtiofsd
// processes that serve its shared folders, and the check that this computer can run KVM at all.
//
// It only builds and checks; it starts nothing. Everything here is plain Go, so its tests run on every system.
package microvm

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Arch is a guest CPU architecture; the guest always has the host's.
type Arch string

const (
	X86_64  Arch = "x86_64"
	AArch64 Arch = "aarch64"
)

// Share is a folder of the computer shown inside the VM, served by its own virtiofsd.
type Share struct {
	Source   string // the folder on the computer
	Target   string // where the guest mounts it
	ReadOnly bool
}

// Spec is one VM.
type Spec struct {
	// QEMU is the path of the qemu-system binary for Arch (the pinned build Home downloads and checks).
	QEMU string
	Arch Arch
	// Name names the VM (letters, digits, '-', '_', '.'); it is QEMU's -name, for logs and the process list.
	Name string
	// Kernel is the guest kernel and Rootfs the guest's root file system: a raw image, attached read-only.
	Kernel string
	Rootfs string
	// CPUs and Memory are the VM's resources: a count, and a size such as "2G" or "1536M" ("" = the defaults).
	CPUs   string
	Memory string
	// Dir is the VM's own folder (only the user can enter it): the sockets of the control channel and of the shares, the
	// pid file and the console log are in it.
	Dir string
	// Shares are the shared folders, in the order the guest mounts them (agent.Project.Mounts).
	Shares []Share
}

// The defaults when the setup wizard gave no limits.
const (
	DefaultCPUs      = 2
	DefaultMemoryMiB = 2048
)

// The names of the files in Spec.Dir.
const (
	ControlSocket = "control.sock" // the control channel (virtio-serial port "rubi.control")
	PidFile       = "qemu.pid"
	ConsoleLog    = "console.log"
	// ControlPort is the name of the virtio-serial port the guest agent opens (/dev/virtio-ports/rubi.control).
	ControlPort = "rubi.control"
)

// ShareSocket is the virtiofsd socket of share i, in Spec.Dir.
func ShareSocket(i int) string { return "fs" + strconv.Itoa(i) + ".sock" }

// ShareTag is the virtio-fs tag of share i: what the guest agent mounts ("mount -t virtiofs rubi0 /work").
func ShareTag(i int) string { return "rubi" + strconv.Itoa(i) }

// GuestMount is one mount the guest agent makes, told over the control channel (the root file system is read-only, so
// the table is not in the image).
type GuestMount struct {
	Tag      string
	Target   string
	ReadOnly bool
}

// GuestMounts is the mount table of spec, in order.
func GuestMounts(spec Spec) []GuestMount {
	ms := make([]GuestMount, len(spec.Shares))
	for i, s := range spec.Shares {
		ms[i] = GuestMount{Tag: ShareTag(i), Target: s.Target, ReadOnly: s.ReadOnly}
	}
	return ms
}

var (
	vmName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	cpuCount   = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)
	memorySize = regexp.MustCompile(`^([1-9][0-9]{0,6})([MG])$`)
)

// ErrSpec is the error of a VM spec that can't be run.
var ErrSpec = errors.New("the VM can't be made")

// KernelCmdline is what the guest kernel is started with: the console on the first serial port, the read-only root on
// the first virtio disk, the guest agent as PID 1, and a panic that powers the VM off (with -no-reboot, QEMU exits).
const KernelCmdline = "console=ttyS0 root=/dev/vda ro rootfstype=ext4 init=/sbin/rubi-guest panic=-1 quiet"

// QEMUArgs is the command line of spec's VM. The VM has no network device at all, no display, no default devices and no
// way to reboot; QEMU runs under its seccomp sandbox and never as root (the caller runs it as the user). Every folder it
// sees comes through a virtiofsd of Shares; the control channel is the only other way in or out.
func QEMUArgs(spec Spec) ([]string, error) {
	if err := spec.check(); err != nil {
		return nil, err
	}
	cpus, mem, err := spec.resources()
	if err != nil {
		return nil, err
	}
	var machine, consoleDev, mmio string
	switch spec.Arch {
	case X86_64:
		// microvm: no PCI, no firmware, no legacy devices but the serial port the console needs
		machine = "microvm,accel=kvm,x-option-roms=off,pit=off,pic=off,rtc=off,isa-serial=on,memory-backend=mem"
		consoleDev = "ttyS0"
		mmio = "device"
	case AArch64:
		machine = "virt,accel=kvm,gic-version=host,memory-backend=mem"
		consoleDev = "ttyAMA0"
		mmio = "pci"
	}
	dir := spec.Dir
	args := []string{
		spec.QEMU,
		"-name", spec.Name,
		"-machine", machine,
		"-cpu", "host",
		"-smp", strconv.Itoa(cpus),
		"-m", strconv.Itoa(mem) + "M",
		// vhost-user (virtiofsd) needs the guest's memory shared with it
		"-object", "memory-backend-memfd,id=mem,size=" + strconv.Itoa(mem) + "M,share=on",
		"-nodefaults", "-no-user-config",
		"-nic", "none",
		"-display", "none",
		"-no-reboot",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-pidfile", filepath.Join(dir, PidFile),
		"-chardev", "file,id=console,path=" + optValue(filepath.Join(dir, ConsoleLog)),
		"-serial", "chardev:console",
		"-kernel", spec.Kernel,
		"-append", strings.Replace(KernelCmdline, "ttyS0", consoleDev, 1),
		"-drive", "file=" + optValue(spec.Rootfs) + ",if=none,id=root,format=raw,readonly=on",
		"-device", "virtio-blk-" + mmio + ",drive=root",
		"-device", "virtio-serial-" + mmio,
		"-chardev", "socket,id=control,path=" + optValue(filepath.Join(dir, ControlSocket)) + ",server=on,wait=off",
		"-device", "virtserialport,chardev=control,name=" + ControlPort,
	}
	for i := range spec.Shares {
		id := "fs" + strconv.Itoa(i)
		args = append(args,
			"-chardev", "socket,id="+id+",path="+optValue(filepath.Join(dir, ShareSocket(i))),
			"-device", "vhost-user-fs-"+mmio+",chardev="+id+",tag="+ShareTag(i))
	}
	return args, nil
}

// optValue escapes a value inside a QEMU option list (key=value,key=value): a comma is written twice.
func optValue(s string) string { return strings.ReplaceAll(s, ",", ",,") }

func (spec Spec) check() error {
	if spec.Arch != X86_64 && spec.Arch != AArch64 {
		return fmt.Errorf("%w: no guest for CPU %q", ErrSpec, spec.Arch)
	}
	if !vmName.MatchString(spec.Name) {
		return fmt.Errorf("%w: the name %q", ErrSpec, spec.Name)
	}
	for what, p := range map[string]string{"QEMU": spec.QEMU, "kernel": spec.Kernel, "root file system": spec.Rootfs, "VM folder": spec.Dir} {
		if err := hostPath(p); err != nil {
			return fmt.Errorf("%w: the %s: %w", ErrSpec, what, err)
		}
	}
	targets := map[string]bool{}
	for i, s := range spec.Shares {
		if err := hostPath(s.Source); err != nil {
			return fmt.Errorf("%w: shared folder %d: %w", ErrSpec, i, err)
		}
		if err := guestPath(s.Target); err != nil {
			return fmt.Errorf("%w: shared folder %d: %w", ErrSpec, i, err)
		}
		if targets[s.Target] {
			return fmt.Errorf("%w: %s is shared twice", ErrSpec, s.Target)
		}
		targets[s.Target] = true
	}
	return nil
}

func (spec Spec) resources() (cpus, memMiB int, err error) {
	cpus, memMiB = DefaultCPUs, DefaultMemoryMiB
	if spec.CPUs != "" {
		if !cpuCount.MatchString(spec.CPUs) {
			return 0, 0, fmt.Errorf("%w: %q CPUs", ErrSpec, spec.CPUs)
		}
		cpus, _ = strconv.Atoi(spec.CPUs)
	}
	if spec.Memory != "" {
		m := memorySize.FindStringSubmatch(strings.ToUpper(spec.Memory))
		if m == nil {
			return 0, 0, fmt.Errorf("%w: memory %q (a size such as 2G or 1536M)", ErrSpec, spec.Memory)
		}
		memMiB, _ = strconv.Atoi(m[1])
		if m[2] == "G" {
			memMiB *= 1024
		}
	}
	if memMiB < 256 || memMiB > 256*1024 {
		return 0, 0, fmt.Errorf("%w: memory %d MiB (256 MiB to 256 GiB)", ErrSpec, memMiB)
	}
	return cpus, memMiB, nil
}

// hostPath accepts an absolute, clean path with no control characters (a newline would end a line of a log or the
// kernel command line, and QEMU reads no other escape than the doubled comma).
func hostPath(p string) error {
	if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("%q is not an absolute, clean path", p)
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%q has a control character", p)
	}
	return nil
}

// guestPath accepts an absolute, clean Linux path inside the guest, never the root itself.
func guestPath(p string) error {
	if !strings.HasPrefix(p, "/") || p == "/" || p != cleanSlash(p) || strings.ContainsAny(p, "\x00\n\r\t ,") {
		return fmt.Errorf("%q is not a place in the VM", p)
	}
	return nil
}

func cleanSlash(p string) string { return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p))) }
