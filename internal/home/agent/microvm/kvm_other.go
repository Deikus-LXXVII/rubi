//go:build !linux

package microvm

import "io/fs"

// HostKVMProbe: KVM is Linux's; elsewhere there is none.
func HostKVMProbe() KVMProbe {
	return KVMProbe{OpenKVM: func() (int, error) { return 0, fs.ErrNotExist }}
}
