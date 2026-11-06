package microvm

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kernelConfig is what the guest kernel's fragments (runner/microvm/kernel) set for arch: option -> "y", or "n" for one
// turned off.
func kernelConfig(t *testing.T, arch Arch) map[string]string {
	t.Helper()
	conf := map[string]string{}
	for _, f := range []string{"common.config", string(arch) + ".config"} {
		fh, err := os.Open(filepath.Join("..", "..", "..", "..", "runner", "microvm", "kernel", f))
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			l := sc.Text()
			if k, v, ok := strings.Cut(l, "="); ok && strings.HasPrefix(k, "CONFIG_") {
				conf[k] = v
			} else if strings.HasPrefix(l, "# CONFIG_") && strings.HasSuffix(l, " is not set") {
				conf[strings.Fields(l)[1]] = "n"
			}
		}
		fh.Close()
	}
	return conf
}

// The guest kernel has, built in, a driver for every device the command line gives the VM, the console and root file
// system the kernel command line names, and what the guest agent mounts; it has no modules (the root is read-only and
// nothing loads them) and no network card driver.
func TestGuestKernelDrivesTheCommandLine(t *testing.T) {
	for _, arch := range []Arch{X86_64, AArch64} {
		spec := roleSpec()
		spec.Arch = arch
		if arch != X86_64 {
			spec.Firmware = ""
		}
		args, err := QEMUArgs(spec)
		if err != nil {
			t.Fatal(err)
		}
		conf := kernelConfig(t, arch)
		need := map[string]string{
			"CONFIG_EXT4_FS": "rootfstype=ext4", "CONFIG_DEVTMPFS": "guest.InitMounts", "CONFIG_TMPFS": "guest.InitMounts",
			"CONFIG_PROC_FS": "guest.InitMounts", "CONFIG_SYSFS": "guest.FindPort", "CONFIG_FUSE_FS": "virtio-fs",
		}
		for _, d := range valuesOf(args, "-device") {
			name := strings.SplitN(d, ",", 2)[0]
			switch {
			case strings.HasSuffix(name, "-device"):
				need["CONFIG_VIRTIO_MMIO"] = name
			case strings.HasSuffix(name, "-pci"):
				need["CONFIG_VIRTIO_PCI"], need["CONFIG_PCI_HOST_GENERIC"] = name, name
			}
			switch {
			case strings.HasPrefix(name, "virtio-blk-"):
				need["CONFIG_VIRTIO_BLK"] = name
			case strings.HasPrefix(name, "virtio-serial-"), name == "virtserialport":
				need["CONFIG_VIRTIO_CONSOLE"] = name
			case strings.HasPrefix(name, "vhost-user-fs-"):
				need["CONFIG_VIRTIO_FS"] = name
			}
		}
		appendArg := valuesOf(args, "-append")[0]
		switch {
		case strings.Contains(appendArg, "console=ttyS0"):
			need["CONFIG_SERIAL_8250"], need["CONFIG_SERIAL_8250_CONSOLE"] = "console=ttyS0", "console=ttyS0"
		case strings.Contains(appendArg, "console=ttyAMA0"):
			need["CONFIG_SERIAL_AMBA_PL011"], need["CONFIG_SERIAL_AMBA_PL011_CONSOLE"] = "console=ttyAMA0", "console=ttyAMA0"
		default:
			t.Errorf("%s: no console the test knows in %q", arch, appendArg)
		}
		if arch == X86_64 {
			need["CONFIG_PVH"] = "qboot starts the kernel through its PVH entry"
		}
		if m := valuesOf(args, "-machine")[0]; strings.Contains(m, "acpi=off") {
			need["CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES"] = "acpi=off: QEMU names the virtio-mmio devices on the command line"
			need["CONFIG_X86_MPPARSE"] = "acpi=off: the interrupt controller is in the MP table"
		}
		for opt, why := range need {
			if conf[opt] != "y" {
				t.Errorf("%s: %s is not built in (%s)", arch, opt, why)
			}
		}
		for _, off := range []string{"CONFIG_MODULES", "CONFIG_NETDEVICES"} {
			if conf[off] != "n" {
				t.Errorf("%s: %s is not turned off", arch, off)
			}
		}
	}
}
