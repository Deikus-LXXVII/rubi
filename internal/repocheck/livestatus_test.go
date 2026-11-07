package repocheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveCompareIgnoresRelayCount: panel.relays_connected moves by itself (a relay
// connects or drops) and must not fail the sandbox isolation check. A real change still fails it.
func TestLiveCompareIgnoresRelayCount(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	sh := filepath.Join(dir, "cmp.sh")
	script := "set -eu\ncd '" + filepath.Join(root(t), "scripts/sandbox") + "'\n. ./lib.sh\n" +
		"sandbox_scrub_status \"$1\" \"$3\"\nsandbox_scrub_status \"$2\" \"$4\"\nsandbox_compare_live \"$3\" \"$4\"\n"
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(before, after string) (string, error) {
		b := filepath.Join(dir, "before.json")
		a := filepath.Join(dir, "after.json")
		if err := os.WriteFile(b, []byte(before), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(a, []byte(after), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", sh, b, a, b+".snap", a+".snap")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	base := `{"state":"unlocked","panel":{"transport":"gateway","relays":4,"relays_connected":6},"link":"http://secret","updated_at":"t1"}`
	flap := `{"state":"unlocked","panel":{"transport":"gateway","relays":4,"relays_connected":5},"link":"http://other","updated_at":"t2"}`
	if out, err := run(base, flap); err != nil {
		t.Fatalf("relay count and other volatile fields should be ignored: %v %s", err, out)
	}
	changed := `{"state":"locked","panel":{"transport":"gateway","relays":4,"relays_connected":6}}`
	out, err := run(base, changed)
	if err == nil {
		t.Fatal("a real status change was treated as unchanged")
	}
	if !strings.Contains(out, "live rubi_status changed") {
		t.Fatalf("compare said %q", out)
	}
}
