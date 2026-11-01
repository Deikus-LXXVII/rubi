package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// RUBI_DUMP_API_BODIES_CHECK=<dir> runs the bodies that the runner's integration tests dumped
// (RUBI_DUMP_API_BODIES, see runner/test/support/mock.mjs) through the gateway policy.
func TestGatewayPolicyAcceptsDumpedClaudeCodeBodies(t *testing.T) {
	dir := os.Getenv("RUBI_DUMP_API_BODIES_CHECK")
	if dir == "" {
		t.Skip("RUBI_DUMP_API_BODIES_CHECK not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	betas := map[string]bool{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, ws := range []bool{true, false} {
			if why := checkBody(b, ws); why != "" && (ws || !strings.Contains(string(b), "web_search")) {
				t.Errorf("%s (webSearch=%v): %s", filepath.Base(f), ws, why)
			}
		}
		bb, _ := os.ReadFile(strings.TrimSuffix(f, ".json") + ".beta")
		for _, v := range strings.Split(string(bb), ",") {
			if v = strings.TrimSpace(v); v != "" {
				betas[v] = true
			}
		}
	}
	var names []string
	for v := range betas {
		names = append(names, v)
	}
	sort.Strings(names)
	kept := map[string]bool{}
	for _, v := range strings.Split(filterBeta([]string{strings.Join(names, ",")}, false), ",") {
		kept[v] = true
	}
	for _, v := range names {
		if !kept[v] {
			t.Errorf("beta %q is dropped", v)
		}
	}
	t.Logf("%d bodies, betas: %v", len(files), names)
}
