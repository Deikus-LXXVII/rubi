package microvm

import (
	"errors"
	"io/fs"
	"strings"
)

// KVMProbe is what the KVM check looks at; the Linux one reads the real computer (kvm_linux.go), tests give their own.
type KVMProbe struct {
	// OpenKVM opens /dev/kvm read-write and returns the KVM API version (KVM_GET_API_VERSION). Its error wraps
	// fs.ErrNotExist when there is no /dev/kvm and fs.ErrPermission when the user may not open it.
	OpenKVM func() (int, error)
	// CPUInfo is /proc/cpuinfo ("" if unreadable).
	CPUInfo func() string
	// Arch is the computer's CPU architecture.
	Arch Arch
}

// KVMAPIVersion is the only KVM API version there has ever been in a released kernel.
const KVMAPIVersion = 12

// The reasons KVM can't be used, the inside of "That isn't available on this computer (...)".
const (
	ReasonNoArch      = "agents run only on x86-64 and 64-bit ARM computers"
	ReasonNoVirt      = "agents need hardware virtualisation, which this computer's CPU doesn't offer to Linux (often so in a VPS or a virtual machine without nested virtualisation)"
	ReasonNoKVM       = "agents need hardware virtualisation (KVM), and this Linux has no /dev/kvm: it may be turned off in the firmware settings, or the kvm module isn't loaded"
	ReasonKVMDenied   = "agents need KVM, and your user may not use /dev/kvm: an administrator of this computer can add you to the kvm group, and you then log in again"
	ReasonKVMOld      = "agents need KVM, and this Linux's KVM is not one Rubi Home knows"
	ReasonKVMUnusable = "agents need KVM, and /dev/kvm could not be used"
)

// CheckKVM says whether this computer can run the VMs: a 64-bit x86 or ARM CPU and a /dev/kvm the user can open that
// speaks the known API. It changes nothing. A missing /dev/kvm is explained by the CPU flags where they say something
// (x86: no vmx or svm flag means the CPU, or the hypervisor under this Linux, doesn't offer virtualisation).
func CheckKVM(p KVMProbe) (ok bool, reason string) {
	if p.Arch != X86_64 && p.Arch != AArch64 {
		return false, ReasonNoArch
	}
	v, err := p.OpenKVM()
	switch {
	case err == nil && v == KVMAPIVersion:
		return true, ""
	case err == nil:
		return false, ReasonKVMOld
	case errors.Is(err, fs.ErrNotExist):
		if p.Arch == X86_64 && p.CPUInfo != nil && !hasVirtFlag(p.CPUInfo()) {
			return false, ReasonNoVirt
		}
		return false, ReasonNoKVM
	case errors.Is(err, fs.ErrPermission):
		return false, ReasonKVMDenied
	}
	return false, ReasonKVMUnusable
}

// hasVirtFlag says whether an x86 /proc/cpuinfo has the vmx (Intel) or svm (AMD) flag; an unreadable one is given the
// benefit of the doubt.
func hasVirtFlag(cpuinfo string) bool {
	if cpuinfo == "" {
		return true
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}
		for _, f := range strings.Fields(v) {
			if f == "vmx" || f == "svm" {
				return true
			}
		}
	}
	return false
}
