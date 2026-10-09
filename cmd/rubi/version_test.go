package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An update checks the new binary by running "rubi version" with a bare environment (only PATH in
// v0.6.0). It must print the version there, or no update from such a Rubi can succeed.
func TestVersionWithBareEnvironment(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "rubi")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "version")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("version with only PATH: %v %q", err, out)
	}
}
