package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSignInFolderChecks(t *testing.T) {
	base := t.TempDir()
	if _, err := EnsureSignInDir("relative/dir"); err == nil {
		t.Error("a relative folder")
	}
	if _, err := EnsureSignInDir(filepath.Join(base, "a,b")); err == nil {
		t.Error("a folder with a comma (it would add mount options)")
	}
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "agent-signin")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureSignInDir(link); err == nil {
		t.Error("the folder is a link")
	}
	dir := filepath.Join(base, "ok")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	real, err := EnsureSignInDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode())
	}
	if SignedInAt(real) {
		t.Error("signed in with no file")
	}
	cred := filepath.Join(real, CredentialFile)
	if err := os.WriteFile(cred, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if SignedInAt(real) {
		t.Error("signed in with an empty file")
	}
	if err := os.WriteFile(cred, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cred, 0o000); err != nil {
		t.Fatal(err)
	}
	if !SignedInAt(real) {
		t.Error("an unreadable credential file isn't seen: the check must only stat it")
	}
	_ = os.Chmod(cred, 0o600)
	env := LoginEnv(real)
	if env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != real || env["CLAUDE_CONFIG_DIR"] != "/tmp/claude-login" || len(env) != 3 {
		t.Errorf("env %v", env)
	}
}

// Home's own folder, where the sign-in folder is, is never a project; neither is anything above or inside it.
func TestProjectsCantTouchTheSignInFolder(t *testing.T) {
	root := t.TempDir()
	helper := filepath.Join(root, "helper")
	signin := filepath.Join(helper, SignInDirName)
	os.MkdirAll(signin, 0o700)
	outside := filepath.Join(root, "my-project")
	os.MkdirAll(outside, 0o755)
	m := New(Options{Dir: helper, Projects: func() []string { return nil }})
	for name, dir := range map[string]string{"sign-in folder": signin, "helper dir": helper, "the parent": root} {
		real, _ := filepath.EvalSymlinks(dir)
		if !m.protected(real) {
			t.Errorf("%s is allowed as a project", name)
		}
	}
	real, _ := filepath.EvalSymlinks(outside)
	if m.protected(real) {
		t.Error("an ordinary folder is protected")
	}
}

// The sign-in container has the proxy for the sign-in hosts and nothing else: no listener answers for service 2 (the auth
// gateway of the earlier design), which stays reserved.
