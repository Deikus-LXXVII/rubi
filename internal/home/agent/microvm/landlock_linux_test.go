//go:build linux

package microvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const confinedHelper = "RUBI_TEST_CONFINED"

// The test binary doubles as the confining helper: with confinedHelper set it confines itself and becomes the program.
func TestMain(m *testing.M) {
	if v := os.Getenv(confinedHelper); v != "" {
		var job struct {
			Rules []FSRule
			Argv  []string
		}
		if err := json.Unmarshal([]byte(v), &job); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		err := ExecConfined(job.Rules, job.Argv, []string{"PATH=/usr/bin:/bin"})
		fmt.Fprintln(os.Stderr, "confined:", err)
		if errors.Is(err, ErrNoLandlock) {
			os.Exit(3)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// A confined program reaches what its rules allow and nothing else: it reads the read-only folder but can't write
// there, writes and removes in the writable one, and can't read or even list anything outside them.
func TestExecConfinedKeepsTheProgramInItsRules(t *testing.T) {
	if _, err := LandlockABI(); errors.Is(err, ErrNoLandlock) {
		t.Skip("no Landlock in this kernel")
	}
	d := t.TempDir()
	ro, rw, secret := filepath.Join(d, "ro"), filepath.Join(d, "rw"), filepath.Join(d, "secret")
	for _, p := range []string{ro, rw, secret} {
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(ro, "f"), []byte("READ-RO-OK\n"), 0o600)
	_ = os.WriteFile(filepath.Join(secret, "s"), []byte("READ-SECRET\n"), 0o600)
	rules := []FSRule{{ro, AccessRead}, {rw, AccessWrite}, {"/usr", AccessExec}}
	for _, l := range []string{"/lib", "/lib64"} {
		if fi, err := os.Lstat(l); err == nil && fi.IsDir() {
			rules = append(rules, FSRule{l, AccessExec})
		}
	}
	// No probe may touch /dev/null: it is outside the rules too, and would fail for that reason alone.
	script := strings.Join([]string{
		"cat " + ro + "/f",
		"echo x 2>&- > " + ro + "/new && echo WROTE-RO",
		"echo y > " + rw + "/g && echo WROTE-RW",
		"mkdir " + rw + "/sub && rmdir " + rw + "/sub && rm " + rw + "/g && echo REMOVED-RW",
		"cat " + secret + "/s 2>&-",
		"ls " + d + " 2>&- > " + rw + "/ls && echo LISTED-PARENT",
		"cat /etc/passwd 2>&- > " + rw + "/etc && echo READ-ETC",
		"echo z 2>&- > /tmp/rubi-confined-$$ && echo WROTE-TMP",
		"cat " + rw + "/../secret/s 2>&-",
		"ln -s " + secret + "/s " + rw + "/link && cat " + rw + "/link 2>&-",
		"true",
	}, "\n")
	job, _ := json.Marshal(map[string]any{"Rules": rules, "Argv": []string{"/bin/sh", "-c", script}})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = []string{confinedHelper + "=" + string(job)}
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		t.Skip("no Landlock")
	}
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	o := string(out)
	for _, want := range []string{"READ-RO-OK", "WROTE-RW", "REMOVED-RW"} {
		if !strings.Contains(o, want) {
			t.Errorf("no %s:\n%s", want, o)
		}
	}
	for _, bad := range []string{"WROTE-RO", "READ-SECRET", "LISTED-PARENT", "READ-ETC", "WROTE-TMP"} {
		if strings.Contains(o, bad) {
			t.Errorf("%s:\n%s", bad, o)
		}
	}
}
