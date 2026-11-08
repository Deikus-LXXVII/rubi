package home

import (
	"errors"
	"os"
	"os/exec"
)

func detach(*exec.Cmd) error {
	return errors.New("on Windows Rubi Home starts at sign-in, with no supervisor of its own")
}

func terminate(p *os.Process) error { return p.Kill() }
