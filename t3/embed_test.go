package t3

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	bridge "github.com/Deikus-LXXVII/rubi/internal/t3"
)

func TestEmbedded(t *testing.T) {
	var up struct {
		Package, Version, Commit, Integrity string
		Runtime                             struct{ SHA256 map[string]string }
		Node                                struct {
			Version string
			SHA256  map[string]string
		}
	}
	if err := json.Unmarshal(Upstream, &up); err != nil {
		t.Fatal(err)
	}
	if up.Package != "t3" || up.Version == "" || up.Commit == "" || up.Integrity == "" {
		t.Errorf("upstream.json = %+v", up)
	}
	// Every platform Home installs on has a pinned Node and T3 runtime archive.
	for _, plat := range []string{"darwin-arm64", "linux-arm64", "linux-x64"} {
		if len(up.Runtime.SHA256[plat]) != 64 || len(up.Node.SHA256[plat]) != 64 {
			t.Errorf("upstream.json has no SHA-256 for %s", plat)
		}
	}
	if !strings.HasPrefix(up.Node.Version, "v") {
		t.Errorf("node version %q", up.Node.Version)
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
	if len(want) != 9 || !reflect.DeepEqual(have, want) {
		t.Errorf("tools.json has %v, the client uses %v", have, want)
	}
}
