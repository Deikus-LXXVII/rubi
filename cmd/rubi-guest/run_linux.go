//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/guest"
	"golang.org/x/sys/unix"
)

func run() {
	log.SetPrefix("rubi-guest: ")
	log.SetFlags(0)
	if len(os.Args) == 2 && os.Args[1] == "serve" {
		os.Exit(serve())
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "rubi-guest: this is the init of Rubi Home's agent VM; it runs only there")
		os.Exit(2)
	}
	if err := guest.InitMounts(); err != nil {
		log.Print(err)
		powerOff()
	}
	cmd := exec.Command("/proc/self/exe", "serve")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	if err := cmd.Start(); err != nil {
		log.Print(err)
		powerOff()
	}
	if code := guest.Reap(cmd.Process.Pid); code != 0 {
		log.Printf("the server ended with %d", code)
	}
	powerOff()
}

func serve() int {
	// Home connects to the port after the VM has started, and may go and come back (a restart of Rubi Home): until it is
	// there, a read gives end of file and a write fails. So the port is opened and served again each time the channel
	// ends (the multiplexer closes the file it served), with a HELLO each time; the VM ends only when Home asks
	// (poweroff) or Home's sweep stops it.
	srv := guest.LinuxServer()
	for {
		dev, err := guest.FindPort("/sys", "/dev", guest.ControlPortName)
		if err == nil {
			var f *os.File
			if f, err = os.OpenFile(dev, os.O_RDWR|unix.O_CLOEXEC, 0); err == nil {
				err = srv.Serve(f)
				_ = f.Close()
			}
		}
		if err != nil {
			log.Printf("the control channel: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// powerOff ends the VM (a restart, which QEMU's -no-reboot turns into its exit).
func powerOff() {
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	os.Exit(1)
}
