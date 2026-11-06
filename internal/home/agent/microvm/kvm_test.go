package microvm

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
)

func probe(arch Arch, v int, err error, cpuinfo string) KVMProbe {
	return KVMProbe{Arch: arch, OpenKVM: func() (int, error) { return v, err }, CPUInfo: func() string { return cpuinfo }}
}

const (
	intelFlags = "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr pae vmx ssse3\n"
	amdFlags   = "processor\t: 0\nflags\t\t: fpu vme svm sse2\n"
	vpsFlags   = "processor\t: 0\nflags\t\t: fpu vme de pse tsc msr pae hypervisor ssse3\n"
)

func TestCheckKVM(t *testing.T) {
	notExist := &fs.PathError{Op: "open", Path: "/dev/kvm", Err: syscall.ENOENT}
	denied := &fs.PathError{Op: "open", Path: "/dev/kvm", Err: syscall.EACCES}
	for name, c := range map[string]struct {
		p      KVMProbe
		ok     bool
		reason string
	}{
		"a desktop with KVM":                 {probe(X86_64, 12, nil, intelFlags), true, ""},
		"an ARM board with KVM":              {probe(AArch64, 12, nil, ""), true, ""},
		"a VPS without nested virt":          {probe(X86_64, 0, notExist, vpsFlags), false, ReasonNoVirt},
		"virtualisation off in the firmware": {probe(X86_64, 0, notExist, amdFlags), false, ReasonNoKVM},
		"an ARM board without KVM":           {probe(AArch64, 0, notExist, ""), false, ReasonNoKVM},
		"unreadable cpuinfo":                 {probe(X86_64, 0, notExist, ""), false, ReasonNoKVM},
		"not in the kvm group":               {probe(X86_64, 0, denied, intelFlags), false, ReasonKVMDenied},
		"an unknown API":                     {probe(X86_64, 13, nil, intelFlags), false, ReasonKVMOld},
		"an ioctl that fails":                {probe(X86_64, 0, syscall.ENOTTY, intelFlags), false, ReasonKVMUnusable},
		"a 32-bit computer":                  {probe("armv7l", 12, nil, ""), false, ReasonNoArch},
	} {
		ok, reason := CheckKVM(c.p)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s: %v %q", name, ok, reason)
		}
	}
	if !errors.Is(denied, fs.ErrPermission) || !errors.Is(notExist, fs.ErrNotExist) {
		t.Fatal("the test's errors are not what os.OpenFile returns")
	}
}
