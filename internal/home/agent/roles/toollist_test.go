package roles

import (
	"os"
	"regexp"
	"testing"
)

// Every tool name Home accepts must be one the runner knows (runner/src/role.ts KNOWN_TOOLS); otherwise the
// runner refuses to start a session of a role Home stored.
func TestKnownToolsMatchTheRunner(t *testing.T) {
	src, err := os.ReadFile("../../../../runner/src/role.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)KNOWN_TOOLS: readonly string\[\] = \[(.*?)\];`).FindSubmatch(src)
	if block == nil {
		t.Fatal("KNOWN_TOOLS not found in runner/src/role.ts")
	}
	runner := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([A-Za-z]+)"`).FindAllSubmatch(block[1], -1) {
		runner[string(m[1])] = true
	}
	for _, name := range KnownTools {
		if !runner[name] {
			t.Errorf("Home accepts tool %q that the runner doesn't know", name)
		}
	}
}
