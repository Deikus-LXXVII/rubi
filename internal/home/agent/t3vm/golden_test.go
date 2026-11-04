package t3vm

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// The golden table of the command lines of a T3 session's VM for Apple's container tool, taken before the VM backend interface
// (docs/plans/home-platforms.md, stage P8). Run with RUBI_UPDATE_GOLDEN=1 only to write a new one on purpose.
const goldenFile = "testdata/session_argv.golden.json"

func goldenSessionArgv(t *testing.T) map[string][]string {
	t.Helper()
	s := testSpec(t)
	s.CPUs, s.Memory = "4", "8G"
	base := strings.TrimSuffix(s.HelperDir, "/h")
	scrub := func(a []string) []string {
		out := make([]string, len(a))
		for i, x := range a {
			out[i] = strings.ReplaceAll(x, base, "BASE")
		}
		return out
	}
	out := map[string][]string{}
	run, err := s.RunArgs()
	if err != nil {
		t.Fatal(err)
	}
	out["run"] = scrub(run)
	c, err := ClampArgv([]string{"--verbose", "--model", "m"}, pol(t3.Auto))
	if err != nil {
		t.Fatal(err)
	}
	ex, err := s.ExecArgs("/h/env/e1", "/opt/claude", c)
	if err != nil {
		t.Fatal(err)
	}
	out["exec"] = scrub(ex)
	out["stop"] = scrub(s.StopArgs())
	out["delete"] = scrub(s.DeleteArgs())
	return out
}

func TestSessionCommandLinesAreTheGoldenTable(t *testing.T) {
	got := goldenSessionArgv(t)
	if os.Getenv("RUBI_UPDATE_GOLDEN") != "" {
		b, _ := json.MarshalIndent(got, "", " ")
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed:\n got  %q\n want %q", got, want)
	}
}
