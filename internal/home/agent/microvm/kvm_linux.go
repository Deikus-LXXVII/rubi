//go:build linux

package microvm

import (
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// kvmGetAPIVersion is KVM_GET_API_VERSION, _IO(0xAE, 0x00).
const kvmGetAPIVersion = 0xAE00

// HostKVMProbe looks at this computer: it opens /dev/kvm (and closes it), asks the API version, and reads /proc/cpuinfo.
func HostKVMProbe() KVMProbe {
	return KVMProbe{
		OpenKVM: func() (int, error) {
			f, err := os.OpenFile("/dev/kvm", os.O_RDWR|unix.O_CLOEXEC, 0)
			if err != nil {
				return 0, err
			}
			defer f.Close()
			v, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), kvmGetAPIVersion, 0)
			if errno != 0 {
				return 0, errno
			}
			return int(v), nil
		},
		CPUInfo: func() string {
			b, err := os.ReadFile("/proc/cpuinfo")
			if err != nil {
				return ""
			}
			return string(b)
		},
		Arch: hostArch(),
	}
}

func hostArch() Arch {
	switch runtime.GOARCH {
	case "amd64":
		return X86_64
	case "arm64":
		return AArch64
	}
	return Arch(runtime.GOARCH)
}
