package repocheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseHasNoVMArtifacts(t *testing.T) {
	s := readRepo(t, ".github/workflows/release.yml")
	for _, v := range []string{"microvm", "rubi-vm-", "virtiofsd"} {
		if strings.Contains(s, v) {
			t.Fatalf("retired VM artifact returned to release: %s", v)
		}
	}
	for _, path := range []string{"../../.github/workflows/microvm.yml", "../../runner/microvm", "../../cmd/rubi-guest", "../../internal/home/agent/microvm", "../../internal/home/agent/guest", "../../internal/home/agent/t3vm", "../../internal/home/agent/apple.go", "../../internal/home/agent/apple_backend.go", "../../runner/embed.go"} {
		if _, e := os.Stat(path); !os.IsNotExist(e) {
			t.Fatalf("retired VM build input returned: %s", path)
		}
	}
}

func TestLegacyBackendFixturesCannotEnterProduction(t *testing.T) {
	err := filepath.WalkDir("../../internal/home", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "agenttest" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "/agent/agenttest") || strings.Contains(string(b), "NewAppleBackend(") {
			t.Errorf("legacy test backend entered production: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
