//go:build linux

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"

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
	dev, err := guest.FindPort("/sys", "/dev", guest.ControlPortName)
	if err != nil {
		log.Print(err)
		return 1
	}
	f, err := os.OpenFile(dev, os.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		log.Print(err)
		return 1
	}
	if err := guest.LinuxServer().Serve(f); err != nil {
		log.Printf("the control channel ended: %v", err)
	}
	return 0
}

func powerOff() {
	unix.Sync()
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	os.Exit(1)
}
