// Package live holds the live test of the Linux VM backend: it boots a real VM with the exact command lines package
// microvm builds, on a computer with KVM, QEMU and virtiofsd. It is only built with the kvmlive tag
// (CGO_ENABLED=0 go test -tags kvmlive ./internal/home/agent/microvm/live: the test binary is the VM's init, so it
// must be static), never in the ordinary test run.
package live
