//go:build !linux

package main

import (
	"fmt"
	"os"
)

func run() {
	fmt.Fprintln(os.Stderr, "rubi-guest: this is the init of Rubi Home's agent VM on Linux; it runs only there")
	os.Exit(2)
}
