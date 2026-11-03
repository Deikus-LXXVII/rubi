package t3

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	bridge "github.com/Deikus-LXXVII/rubi/internal/t3"
)

func TestEmbedded(t *testing.T) {
	var up struct {
		Package, Version, Commit, Integrity string
	}
	if err := json.Unmarshal(Upstream, &up); err != nil {
		t.Fatal(err)
	}
	if up.Package != "t3" || up.Version == "" || up.Commit == "" || up.Integrity == "" {
		t.Errorf("upstream.json = %+v", up)
	}
	b, err := Contract.ReadFile("contract/tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var tools map[string]any
	if err := json.Unmarshal(b, &tools); err != nil {
		t.Fatalf("tools.json: %v", err)
	}
	// The snapshot covers exactly the tools the client calls, so the two cannot drift apart unnoticed.
	var have []string
	for n, schema := range tools {
		have = append(have, n)
		if m, ok := schema.(map[string]any); !ok || m["type"] != "object" {
			t.Errorf("%s has no input schema in tools.json", n)
		}
	}
	want := append([]string(nil), bridge.UsedTools...)
	sort.Strings(have)
	sort.Strings(want)
	if len(want) != 8 || !reflect.DeepEqual(have, want) {
		t.Errorf("tools.json has %v, the client uses %v", have, want)
	}
}
