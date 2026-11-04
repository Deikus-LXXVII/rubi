package home

import (
	"context"
	"testing"
)

// On Linux the system's Elevator is pkexec, by its absolute path.
func TestLinuxSystemElevatorIsPkexec(t *testing.T) {
	e := &elevRun{}
	el := NewElevator(ElevateEnv{Exec: e.exec, LookPath: lookIn("/usr/bin/pkexec"), Getenv: envOf("DISPLAY", ":0"), Geteuid: func() int { return 1000 }})
	if err := el.Run(context.Background(), InstallPlan{Script: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(e.calls) != 1 || e.calls[0].name != "/usr/bin/pkexec" {
		t.Errorf("%+v", e.calls)
	}
}
