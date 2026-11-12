package home

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// TestNativeLoginStepIsOffered is the native (no LoginAgents) path: the setup page
// offers the shared Claude sign-in, not "unsupported" / "VM sign-in was retired".
func TestNativeLoginStepIsOffered(t *testing.T) {
	l := newLife(t)
	putRole(t, l.h, "reviewer")
	l.t3.AgentSetup = func(ctx context.Context, op string, args json.RawMessage) (any, error) {
		return map[string]any{"steps": []any{map[string]any{"id": "login"}, map[string]any{"id": "selftest"}}, "job": nil, "login": nil}, nil
	}
	if l.t3.LoginAgents != nil {
		t.Fatal("fixture set LoginAgents; this test is the native path")
	}
	st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	login := stepByID(t, st, "login")
	if login["state"] == "unsupported" {
		t.Fatalf("native sign-in is still closed: %v", login)
	}
	if login["state"] != "todo" {
		t.Fatalf("before a credential: %v", login)
	}
	// The Sign in button is held back while earlier T3 steps are not done
	// (noteLoginWaiting). That is existing setup-page behaviour, not a refusal.

	dir := agent.SignInDirOf(l.h.Dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.CredentialFile), []byte(`{"fixture":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	st, err = handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	login = stepByID(t, st, "login")
	if login["state"] != "done" || login["action"] != "login_again" {
		t.Fatalf("with a credential file: %v", login)
	}

	// Starting or finishing a sign-in is no longer refused as "retired".
	_, err = handle(t, l.h, "tok", "t3-setup.login.open", nil)
	if err == nil || strings.Contains(err.Error(), "retired") {
		t.Errorf("login.open: %v", err)
	}
	_, err = handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "ABC"})
	if err == nil || strings.Contains(err.Error(), "retired") {
		t.Errorf("login.code: %v", err)
	}
}

func fmtString(v any) string {
	s, _ := v.(string)
	return s
}

// TestEveryProjectLaunchUsesTheServerLoginDir: every project's srt policy mounts
// the same server-level folder, never a project state dir or a per-role folder.
func TestEveryProjectLaunchUsesTheServerLoginDir(t *testing.T) {
	b, _, path, _ := sandboxFixture(t)
	// A second role, same helper, different project.
	other := filepath.Join(filepath.Dir(b.HomeDir), "project-b")
	r2 := roles.Role{ID: "r_aaaaaa02", Project: "project-b", DirHash: roles.DirHash(other), Status: "active", Version: 1}
	rd := filepath.Join(b.HomeDir, agent.RolesDir, r2.ID)
	writeSandboxJSON(t, filepath.Join(rd, "versions", "1.json"), r2)
	writeSandboxJSON(t, filepath.Join(rd, "index.json"), r2)
	b2 := b
	b2.Role, b2.Project, b2.DirHash = r2.ID, r2.Project, r2.DirHash
	path2 := filepath.Join(b.HomeDir, "t3", "sandbox", r2.ID, "claudeAgent", "binding.json")

	l1, err := ResolveAgentLaunch(b, path)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := ResolveAgentLaunch(b2, path2)
	if err != nil {
		t.Fatal(err)
	}
	want, err := agent.EnsureSignInDir(agent.SignInDirOf(b.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if l1.Policy.Login != want || l2.Policy.Login != want {
		t.Fatalf("projects got different or non-shared login dirs: %q %q, want %q", l1.Policy.Login, l2.Policy.Login, want)
	}
	for _, got := range []string{l1.Policy.Login, l2.Policy.Login} {
		if got == "" || strings.Contains(got, string(filepath.Separator)+"state") || strings.Contains(got, agent.RolesDir) {
			t.Fatalf("login dir looks like project or role state: %q", got)
		}
	}
	st, err := os.Lstat(want)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("shared login dir is not a private 0700 folder: %v %v", st, err)
	}
}

// TestProjectCodeDoesNotPointLoginAtProjectState fails if t3project.go assigns
// Policy.Login to the per-project state folder again.
func TestProjectCodeDoesNotPointLoginAtProjectState(t *testing.T) {
	src, err := os.ReadFile("t3project.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`Policy\.Login\s*=\s*state\b`).Match(src) {
		t.Error("t3project.go sets Policy.Login to the project state dir; every project must use the server-level sign-in folder")
	}
	if !regexp.MustCompile(`Policy\.Login\s*=`).Match(src) {
		t.Error("t3project.go no longer sets Policy.Login")
	}
	if !strings.Contains(string(src), "signInDir") && !strings.Contains(string(src), "SignInDirOf") && !strings.Contains(string(src), "EnsureSignInDir") {
		t.Error("t3project.go does not point Policy.Login at the shared sign-in folder")
	}
}
