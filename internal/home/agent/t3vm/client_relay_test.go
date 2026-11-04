package t3vm

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Where a process cannot be replaced by another (Windows), the shim runs the command with its own input and output, keeps
// the connection to Home open for as long as it runs, and returns its exit status. The relay itself is plain os/exec and is
// tested here on every system that has sh.
func TestRelayProcessPassesInputOutputAndExitStatus(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	var out, errb bytes.Buffer
	code, err := relayProcess(sh, []string{"sh", "-c", "cat; echo to-stderr >&2; echo \"$RELAY_VAR\"; exit 7"}, []string{"RELAY_VAR=from-env", "PATH=/usr/bin:/bin"},
		strings.NewReader("hello from stdin\n"), &out, &errb)
	if err != nil || code != 7 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.String() != "hello from stdin\nfrom-env\n" || errb.String() != "to-stderr\n" {
		t.Fatalf("stdout %q stderr %q", out.String(), errb.String())
	}
	if code, err := relayProcess(sh, []string{"sh", "-c", "exit 0"}, nil, strings.NewReader(""), &out, &errb); err != nil || code != 0 {
		t.Fatalf("success: %d %v", code, err)
	}
	// a program that cannot be started is an error, with status 1
	if code, err := relayProcess("/nonexistent/program", []string{"program"}, nil, strings.NewReader(""), &out, &errb); err == nil || code != 1 {
		t.Fatalf("missing program: %d %v", code, err)
	}
	if code, err := relayProcess(sh, nil, nil, strings.NewReader(""), &out, &errb); err == nil || code != 1 {
		t.Fatalf("no argv: %d %v", code, err)
	}
	// it does not write to the streams on its own
	out.Reset()
	errb.Reset()
	_, _ = relayProcess(sh, []string{"sh", "-c", "true"}, nil, strings.NewReader(""), &out, &errb)
	if out.Len() != 0 || errb.Len() != 0 {
		t.Fatalf("the relay wrote %q %q", out.String(), errb.String())
	}
}
