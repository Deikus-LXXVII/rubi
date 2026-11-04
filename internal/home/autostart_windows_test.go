package home

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The real Task Scheduler accepts the definition: a task of another name is created from the XML, found, started,
// restarted and deleted. (The CI runner is an administrator, so this proves the definition and the calls, not that a
// standard user may do it; that is checked by hand on a standard account.)
func TestWindowsTaskSchedulerAcceptsTheDefinition(t *testing.T) {
	sys := os.Getenv("SystemRoot") + `\System32`
	if _, err := os.Stat(sys + `\schtasks.exe`); err != nil {
		t.Skip("no schtasks")
	}
	out, err := exec.Command("whoami").Output()
	if err != nil {
		t.Skip(err)
	}
	user := strings.TrimSpace(string(out))
	env := AutostartEnv{SystemDir: sys, User: user, TaskName: "Rubi Home CI test"}.withDefaults()
	as := schtasksAutostart(env)
	t.Cleanup(func() { _ = as.Remove(context.Background()) })
	logs := t.TempDir() + `\logs`
	if _, err := as.Install(context.Background(), AutostartSpec{Bin: sys + `\whoami.exe`, Logs: logs}); err != nil {
		t.Fatal(err)
	}
	if !as.Loaded(context.Background()) {
		t.Error("the task is not there after Install")
	}
	if err := as.Restart(context.Background()); err != nil {
		t.Error(err)
	}
	if err := as.Remove(context.Background()); err != nil {
		t.Error(err)
	}
	if as.Loaded(context.Background()) {
		t.Error("the task is still there after Remove")
	}
}
