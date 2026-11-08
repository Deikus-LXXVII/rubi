package home

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Mixed-version installs must never launch an old untracked shim and later
// interpret an empty receipt directory as proof that it stopped.
func (t *T3Home) verifyTaskSupervisor(ctx context.Context) error {
	helper, err := t.publisherHelper()
	if err != nil {
		return errors.New("task: install the current rubi-agent-exec alongside Rubi Home before starting tasks")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper, "--task-supervision-version")
	cmd.Dir = t.h.Dir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stderr = io.Discard
	var out supervisorOutput
	cmd.Stdout = &out
	if cmd.Run() != nil || strings.TrimSpace(out.text) != "1" {
		return errors.New("task: rubi-agent-exec lacks task supervision v1; update the bundled helper (no untracked fallback)")
	}
	return nil
}

type supervisorOutput struct{ text string }

func (o *supervisorOutput) Write(p []byte) (int, error) {
	if len(o.text)+len(p) > 64 {
		return 0, errors.New("supervisor capability response too large")
	}
	o.text += string(p)
	return len(p), nil
}
