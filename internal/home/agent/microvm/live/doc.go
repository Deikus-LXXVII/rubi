// Package live holds the live test of the Linux VM backend: it boots a real VM with the exact command lines package
// microvm builds, on a computer with KVM, QEMU and virtiofsd. It is only built with the kvmlive tag
// (go test -tags kvmlive ./internal/home/agent/microvm/live), never in the ordinary test run.
package live
