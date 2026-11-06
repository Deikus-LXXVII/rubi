package setup

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The golden table of every command line the setup wizard gives Apple's container tool (docs/plans/home-platforms.md, stage
// P8): taken from the code before the VM backend interface, so that the move is shown to change nothing on a Mac. Run with
// RUBI_UPDATE_GOLDEN=1 only to write a new one on purpose.
const goldenFile = "testdata/container_argv.golden.json"

func goldenSetupArgv(t *testing.T) map[string][][]string {
	t.Helper()
	out := map[string][][]string{}
	take := func(k string, e *testEnv) {
		e.cmd.mu.Lock()
		defer e.cmd.mu.Unlock()
		for _, c := range e.cmd.calls {
			out[k] = append(out[k], append([]string(nil), c...))
		}
		e.cmd.calls = nil
	}
	ctx := context.Background()

	// a Mac with nothing: the status probes
	e := newTestEnv(t)
	e.s.Status(ctx)
	take("status_nothing", e)

	// a ready Mac: the same probes
	e = newTestEnv(t)
	e.ready()
	e.s.Status(ctx)
	take("status_ready", e)

	// the service step, then the image step, on a Mac that has the tool but nothing running
	e = newTestEnv(t)
	e.ready()
	running := e.m.sysJSON
	e.m.sysJSON = "" // not running until it is started
	e.m.extra = func(argv []string) ([]byte, error, bool) {
		if strings.HasPrefix(strings.Join(argv, " "), "container system start") {
			e.m.sysJSON = running
			return []byte("starting"), nil, true
		}
		return nil, nil, false
	}
	if err := e.s.Run(ctx, StepService, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	take("run_service", e)

	e = newTestEnv(t)
	e.ready()
	e.m.image = false
	if err := e.s.Run(ctx, StepImage, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	take("run_image", e)
	for k, v := range out {
		for i, c := range v {
			for j := range c {
				c[j] = strings.ReplaceAll(c[j], e.dir, "HELPER")
			}
			out[k][i] = c
		}
	}
	return out
}

func TestSetupCommandLinesAreTheGoldenTable(t *testing.T) {
	got := goldenSetupArgv(t)
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
	var want map[string][][]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Errorf("%d sequences, golden has %d", len(got), len(want))
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s changed:\n got  %q\n want %q", k, got[k], w)
		}
	}
}
