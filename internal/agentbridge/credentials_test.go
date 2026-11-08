package agentbridge

import (
	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateProcAttestation(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
	}{{"NSpid:\t12\n", true}, {"NSpid:\t1 12\n", false}, {"NSpid:\t13\n", false}, {"Name:\tinit\n", false}} {
		if privateProc(tc.status, 12) != tc.want {
			t.Fatal("incorrect private proc attestation")
		}
	}
}
func TestCredentialsRemainOutsideAgentWrites(t *testing.T) {
	root := t.TempDir()
	p := agentsandbox.Policy{Projects: []string{filepath.Join(root, "project")}, Home: filepath.Join(root, "home"), Login: filepath.Join(root, "login"), Tmp: filepath.Join(root, "tmp")}
	for _, d := range []string{p.Projects[0], p.Home, p.Login, p.Tmp} {
		os.Mkdir(d, 0700)
	}
	file := filepath.Join(root, "credentials.json")
	os.WriteFile(file, []byte(`{"CURSOR_API_KEY":"synthetic-only"}`), 0600)
	p.SensitiveRoots = []string{file}
	if v, err := Credentials(p, file); err != nil || v["CURSOR_API_KEY"] != "synthetic-only" {
		t.Fatal("private credential file refused", err)
	}
	bad := filepath.Join(p.Projects[0], "credentials.json")
	os.WriteFile(bad, []byte(`{"CURSOR_API_KEY":"synthetic-only"}`), 0600)
	if _, err := Credentials(p, bad); err == nil {
		t.Fatal("writable credential file accepted")
	}
	os.Chmod(file, 0644)
	if _, err := Credentials(p, file); err == nil {
		t.Fatal("public credential file accepted")
	}
	os.Chmod(file, 0600)
	os.WriteFile(file, []byte(`{"NODE_OPTIONS":"unsafe-loader"}`), 0600)
	if _, err := Credentials(p, file); err == nil {
		t.Fatal("loader injection accepted")
	}
	os.WriteFile(file, []byte(`{"CURSOR_API_KEY":`), 0600)
	if _, err := Credentials(p, file); err == nil {
		t.Fatal("malformed credential file accepted")
	}
}

func TestCredentialsRejectNullReservedAndAliases(t *testing.T) {
	root := t.TempDir()
	p := agentsandbox.Policy{Projects: []string{filepath.Join(root, "p")}, Home: filepath.Join(root, "h"), Login: filepath.Join(root, "l"), Tmp: filepath.Join(root, "t")}
	for _, d := range []string{p.Projects[0], p.Home, p.Login, p.Tmp} {
		os.Mkdir(d, 0700)
	}
	file := filepath.Join(root, "key.json")
	p.SensitiveRoots = []string{file}
	for _, data := range []string{`null`, `{"T3CODE_AUTH_TOKEN":"synthetic"}`, `{"RUBI_API_KEY":"synthetic"}`} {
		os.WriteFile(file, []byte(data), 0600)
		if _, err := Credentials(p, file); err == nil {
			t.Fatal("unsafe credential map accepted")
		}
	}
	os.WriteFile(file, []byte(`{"CURSOR_API_KEY":"synthetic"}`), 0600)
	alias := filepath.Join(p.Projects[0], "alias")
	os.Symlink(file, alias)
	if _, err := Credentials(p, alias); err == nil {
		t.Fatal("agent-writable credential alias accepted")
	}
	p.RuntimeRead = []string{root}
	if _, err := Credentials(p, file); err == nil {
		t.Fatal("credential read exception accepted")
	}
}
func TestStateEnvironmentCannotEscapeThroughSymlink(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	os.Symlink(outside, filepath.Join(state, "home"))
	if _, err := StateEnvironment(state); err == nil {
		t.Fatal("state symlink escaped root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("host path changed")
	}
}
