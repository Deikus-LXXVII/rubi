//go:build darwin

package home

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The argv of the administrator run: the script and the prompt are arguments of the run handler (nothing is
// spliced into the AppleScript), and the dialog names who is asking.
func TestOsascriptArgvCarriesTheScriptAndThePrompt(t *testing.T) {
	p := InstallPlan{Script: "echo hi '\"' $x"}
	a := osascriptArgs(p)
	if len(a) != 8 || a[0] != "-e" || a[1] != "on run argv" || a[6] != p.FullScript() || a[7] != adminPrompt {
		t.Fatalf("argv: %q", a)
	}
	if !strings.Contains(a[3], "with prompt (item 2 of argv)") || !strings.Contains(a[3], "(item 1 of argv)") {
		t.Fatalf("handler: %q", a[3])
	}
	for _, x := range a[:6] {
		if strings.Contains(x, "echo hi") || strings.Contains(x, "Rubi Home wants") {
			t.Fatalf("data spliced into AppleScript: %q", x)
		}
	}
	if !strings.Contains(adminPrompt, "Rubi Home wants to install the Claude Code hook") {
		t.Fatal(adminPrompt)
	}
}

type execCall struct {
	env  []string
	name string
	args []string
}

// macExec is a fake osascript: it records the call and answers with out and err.
func macExec(calls *[]execCall, out string, err error) ElevExec {
	return func(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, execCall{env, name, args})
		return []byte(out), err
	}
}

// The macOS form is what it always was: osascript with exactly osascriptArgs and the one-entry scrubbed environment.
func TestMacElevatorIsTheOsascriptRunItAlwaysWas(t *testing.T) {
	var calls []execCall
	p := InstallPlan{Script: "set -e; echo hi"}
	e := NewElevator(ElevateEnv{Exec: macExec(&calls, "", nil)})
	if err := e.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []execCall{{[]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}, "/usr/bin/osascript", osascriptArgs(p)}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("%+v", calls)
	}
	// the golden argv, spelled out
	golden := []string{"-e", "on run argv",
		"-e", "do shell script (item 1 of argv) with administrator privileges with prompt (item 2 of argv)",
		"-e", "end run", "set -e; echo hi", "Rubi Home wants to install the Claude Code hook (approved in your Rubi panel)."}
	if !reflect.DeepEqual(calls[0].args, golden) {
		t.Fatalf("argv %q", calls[0].args)
	}
	if e.Describe() != `/usr/bin/osascript, "do shell script ... with administrator privileges" (macOS asks for an administrator password)` {
		t.Errorf("Run via: %q", e.Describe())
	}
	if !strings.Contains(e.Sentence(), "macOS will ask for the administrator password") {
		t.Errorf("sentence %q", e.Sentence())
	}
	if e.Available(context.Background()) != nil {
		t.Error("macOS can always ask")
	}
}

// Cancel (error -128, "User canceled") is the existing clear result; any other failure is an error with the output.
func TestMacElevatorCancelAndFailure(t *testing.T) {
	for _, out := range []string{"0:59: execution error: User canceled. (-128)", "User canceled"} {
		var calls []execCall
		err := NewElevator(ElevateEnv{Exec: macExec(&calls, out, errors.New("exit status 1"))}).Run(context.Background(), InstallPlan{})
		if !errors.Is(err, ErrAdminCancelled) {
			t.Errorf("%q: %v", out, err)
		}
	}
	var calls []execCall
	err := NewElevator(ElevateEnv{Exec: macExec(&calls, "boom", errors.New("exit status 1"))}).Run(context.Background(), InstallPlan{})
	if err == nil || errors.Is(err, ErrAdminCancelled) || !strings.Contains(err.Error(), "osascript") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("%v", err)
	}
}
