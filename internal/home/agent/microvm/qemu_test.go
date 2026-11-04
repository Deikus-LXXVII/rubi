package microvm

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func roleSpec() Spec {
	return Spec{
		QEMU: "/opt/rubi/qemu/bin/qemu-system-x86_64", Arch: X86_64, Name: "rubi-web-app-reviewer",
		Kernel: "/opt/rubi/vm/vmlinux", Rootfs: "/opt/rubi/vm/rootfs.img", Dir: "/home/u/.local/state/rubi-home/vm/web-app",
		Shares: []Share{
			{Source: "/home/u/src/web-app", Target: "/work"},
			{Source: "/home/u/.local/state/rubi-home/empty", Target: "/work/.rubi", ReadOnly: true},
			{Source: "/home/u/.local/state/rubi-home/roles/reviewer", Target: "/rubi-role", ReadOnly: true},
			{Source: "/home/u/.local/state/rubi-home/claude/reviewer", Target: "/rubi-claude"},
		},
	}
}

// valueOf is the value after each occurrence of flag.
func valuesOf(args []string, flag string) []string {
	var out []string
	for i := 1; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

// The whole command line of a role's VM on x86-64, literally.
func TestQEMUArgsOfARoleVM(t *testing.T) {
	got, err := QEMUArgs(roleSpec())
	if err != nil {
		t.Fatal(err)
	}
	d := "/home/u/.local/state/rubi-home/vm/web-app/"
	want := []string{"/opt/rubi/qemu/bin/qemu-system-x86_64",
		"-name", "rubi-web-app-reviewer",
		"-machine", "microvm,accel=kvm,x-option-roms=off,pit=off,pic=off,rtc=off,isa-serial=on,memory-backend=mem",
		"-cpu", "host", "-smp", "2", "-m", "2048M",
		"-object", "memory-backend-memfd,id=mem,size=2048M,share=on",
		"-nodefaults", "-no-user-config", "-nic", "none", "-display", "none", "-no-reboot",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-pidfile", d + "qemu.pid",
		"-chardev", "file,id=console,path=" + d + "console.log", "-serial", "chardev:console",
		"-kernel", "/opt/rubi/vm/vmlinux",
		"-append", "console=ttyS0 root=/dev/vda ro rootfstype=ext4 init=/sbin/rubi-guest panic=-1 quiet",
		"-drive", "file=/opt/rubi/vm/rootfs.img,if=none,id=root,format=raw,readonly=on",
		"-device", "virtio-blk-device,drive=root",
		"-device", "virtio-serial-device",
		"-chardev", "socket,id=control,path=" + d + "control.sock,server=on,wait=off",
		"-device", "virtserialport,chardev=control,name=rubi.control",
		"-chardev", "socket,id=fs0,path=" + d + "fs0.sock", "-device", "vhost-user-fs-device,chardev=fs0,tag=rubi0",
		"-chardev", "socket,id=fs1,path=" + d + "fs1.sock", "-device", "vhost-user-fs-device,chardev=fs1,tag=rubi1",
		"-chardev", "socket,id=fs2,path=" + d + "fs2.sock", "-device", "vhost-user-fs-device,chardev=fs2,tag=rubi2",
		"-chardev", "socket,id=fs3,path=" + d + "fs3.sock", "-device", "vhost-user-fs-device,chardev=fs3,tag=rubi3",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
}

// What makes the boundary, checked by property so that a later edit of the list can't drop one: no network device of any
// kind, the seccomp sandbox with every deny, no default devices or config file, no display, no reboot, a read-only root,
// and no way to reach the host's files but the virtio-fs shares.
func TestQEMUArgsKeepTheBoundary(t *testing.T) {
	for _, arch := range []Arch{X86_64, AArch64} {
		spec := roleSpec()
		spec.Arch = arch
		args, err := QEMUArgs(spec)
		if err != nil {
			t.Fatal(err)
		}
		if n := valuesOf(args, "-nic"); !reflect.DeepEqual(n, []string{"none"}) {
			t.Errorf("%s: -nic %q", arch, n)
		}
		for _, a := range args[1:] {
			if slices.Contains([]string{"-netdev", "-net", "-virtfs", "-fsdev", "-usb", "-usbdevice", "-vnc", "-spice", "-monitor", "-qmp", "-mon",
				"-gdb", "-s", "-S", "-loadvm", "-incoming", "-fw_cfg", "-dtb", "-bios", "-hda", "-cdrom", "-runas", "-chroot", "-run-with", "-enable-kvm"}, a) {
				t.Errorf("%s: %q in the command line", arch, a)
			}
			l := strings.ToLower(a)
			for _, bad := range []string{"virtio-net", "e1000", "rtl8139", "vmxnet", "netdev", "hostfwd", "9p", "usb", "vfio", "vhost-vsock", "ivshmem", "pflash"} {
				if strings.Contains(l, bad) {
					t.Errorf("%s: %q in the command line", arch, a)
				}
			}
		}
		if s := valuesOf(args, "-sandbox"); len(s) != 1 || s[0] != "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny" {
			t.Errorf("%s: -sandbox %q", arch, s)
		}
		for _, f := range []string{"-nodefaults", "-no-user-config", "-no-reboot"} {
			if !slices.Contains(args, f) {
				t.Errorf("%s: no %s", arch, f)
			}
		}
		if d := valuesOf(args, "-display"); !reflect.DeepEqual(d, []string{"none"}) {
			t.Errorf("%s: -display %q", arch, d)
		}
		drives := valuesOf(args, "-drive")
		if len(drives) != 1 || !strings.HasSuffix(drives[0], ",readonly=on") {
			t.Errorf("%s: drives %q", arch, drives)
		}
		var fs int
		for _, d := range valuesOf(args, "-device") {
			if strings.HasPrefix(d, "vhost-user-fs-") {
				fs++
			}
		}
		if fs != len(spec.Shares) {
			t.Errorf("%s: %d shares for %d folders", arch, fs, len(spec.Shares))
		}
	}
}

func TestQEMUArgsOnARM(t *testing.T) {
	spec := roleSpec()
	spec.Arch = AArch64
	spec.QEMU = "/opt/rubi/qemu/bin/qemu-system-aarch64"
	args, err := QEMUArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if m := valuesOf(args, "-machine"); !reflect.DeepEqual(m, []string{"virt,accel=kvm,gic-version=host,memory-backend=mem"}) {
		t.Errorf("machine %q", m)
	}
	if a := valuesOf(args, "-append"); len(a) != 1 || !strings.HasPrefix(a[0], "console=ttyAMA0 ") {
		t.Errorf("append %q", a)
	}
	for _, d := range valuesOf(args, "-device") {
		if strings.HasSuffix(strings.SplitN(d, ",", 2)[0], "-device") {
			t.Errorf("a virtio-mmio device on the virt machine: %q", d)
		}
	}
}

// A comma in a path is doubled wherever QEMU reads it inside an option list, so a folder named "a,share=off" can't add
// an option; paths that are plain arguments are passed as they are.
func TestQEMUArgsEscapeCommasInPaths(t *testing.T) {
	spec := roleSpec()
	spec.Dir = "/home/u/vm,share=off"
	spec.Rootfs = "/home/u/root,readonly=off.img"
	args, err := QEMUArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if d := valuesOf(args, "-drive")[0]; d != "file=/home/u/root,,readonly=off.img,if=none,id=root,format=raw,readonly=on" {
		t.Errorf("drive %q", d)
	}
	for _, c := range valuesOf(args, "-chardev") {
		if !strings.Contains(c, "path=/home/u/vm,,share=off/") {
			t.Errorf("chardev %q", c)
		}
	}
	if p := valuesOf(args, "-pidfile")[0]; p != "/home/u/vm,share=off/qemu.pid" {
		t.Errorf("pidfile %q", p)
	}
}

func TestQEMUArgsResources(t *testing.T) {
	for _, c := range []struct{ cpus, mem, smp, m string }{
		{"", "", "2", "2048M"}, {"4", "8G", "4", "8192M"}, {"1", "1536m", "1", "1536M"}, {"16", "256M", "16", "256M"},
	} {
		spec := roleSpec()
		spec.CPUs, spec.Memory = c.cpus, c.mem
		args, err := QEMUArgs(spec)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if valuesOf(args, "-smp")[0] != c.smp || valuesOf(args, "-m")[0] != c.m || !strings.Contains(valuesOf(args, "-object")[0], "size="+c.m+",") {
			t.Errorf("%+v: %q %q %q", c, valuesOf(args, "-smp"), valuesOf(args, "-m"), valuesOf(args, "-object"))
		}
	}
	for _, c := range []struct{ cpus, mem string }{{"0", ""}, {"-1", ""}, {"2.5", ""}, {"1000", ""}, {"", "2"}, {"", "2T"}, {"", "100M"}, {"", "0G"}, {"", "2G,share=off"}, {"2 ", ""}} {
		spec := roleSpec()
		spec.CPUs, spec.Memory = c.cpus, c.mem
		if _, err := QEMUArgs(spec); !errors.Is(err, ErrSpec) {
			t.Errorf("%+v accepted: %v", c, err)
		}
	}
}

func TestQEMUArgsRefuseWhatCantBeRun(t *testing.T) {
	for name, mutate := range map[string]func(*Spec){
		"unknown CPU":            func(s *Spec) { s.Arch = "riscv64" },
		"empty name":             func(s *Spec) { s.Name = "" },
		"name with a comma":      func(s *Spec) { s.Name = "a,b" },
		"name with a space":      func(s *Spec) { s.Name = "a b" },
		"relative QEMU":          func(s *Spec) { s.QEMU = "qemu-system-x86_64" },
		"relative kernel":        func(s *Spec) { s.Kernel = "vmlinux" },
		"unclean root":           func(s *Spec) { s.Rootfs = "/opt/rubi/../rootfs.img" },
		"no VM folder":           func(s *Spec) { s.Dir = "" },
		"newline in a folder":    func(s *Spec) { s.Shares[0].Source = "/home/u/a\nb" },
		"relative shared folder": func(s *Spec) { s.Shares[0].Source = "src" },
		"guest root":             func(s *Spec) { s.Shares[0].Target = "/" },
		"relative guest place":   func(s *Spec) { s.Shares[0].Target = "work" },
		"unclean guest place":    func(s *Spec) { s.Shares[1].Target = "/work/../etc" },
		"comma in guest place":   func(s *Spec) { s.Shares[1].Target = "/work/a,b" },
		"shared twice":           func(s *Spec) { s.Shares[3].Target = "/work" },
	} {
		spec := roleSpec()
		spec.Shares = slices.Clone(spec.Shares)
		mutate(&spec)
		if args, err := QEMUArgs(spec); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: accepted, %q", name, args)
		}
	}
}

func TestGuestMountsFollowTheShares(t *testing.T) {
	got := GuestMounts(roleSpec())
	want := []GuestMount{{"rubi0", "/work", false}, {"rubi1", "/work/.rubi", true}, {"rubi2", "/rubi-role", true}, {"rubi3", "/rubi-claude", false}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
}
