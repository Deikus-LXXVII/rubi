package home

import (
	"errors"
	"os"
	"os/exec"
)

func detach(*exec.Cmd) error {
	return errors.New("rubi-home start isn't for Windows; Rubi Home starts at sign-in there")
}

func terminate(p *os.Process) error { return p.Kill() }
