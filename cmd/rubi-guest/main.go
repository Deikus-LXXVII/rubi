// Command rubi-guest is the guest agent of an agent's VM on Linux (docs/design/linux-microvm.md): the VM's PID 1.
//
//	rubi-guest          as PID 1 (the kernel's init=): mounts /proc, /sys, /dev, /tmp and /run, starts "rubi-guest serve"
//	                    and reaps every orphan until it ends, then powers the VM off
//	rubi-guest serve    serves Home's requests on the control channel (package guest)
//
// It runs only inside the VM; elsewhere it says so and exits.
package main

func main() { run() }
