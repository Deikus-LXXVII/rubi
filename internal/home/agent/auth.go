package agent

// Legacy credential-directory compatibility; no VM or container may be started.
import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type AuthSession struct{}

func (*AuthSession) Exec(context.Context, []string, map[string]string) (Conn, error) {
	return nil, errors.New("VM sign-in was retired; provision native provider credentials explicitly")
}
func (*AuthSession) Close() {}

const (
	// LoginContainerPrefix starts the name of the container a T3 roles' sign-in runs in; each sign-in adds a random suffix, so
	// one that ends deletes only its own.
	LoginContainerPrefix = "rubi-t3-login-"
	// LoginHelper is the helper in the image that runs `claude auth login` (runner/container/rubi-claude-login).
	LoginHelper = "/usr/local/bin/rubi-claude-login"
	// SignInDirName is the folder every T3 role's claude shares for its sign-in (CLAUDE_SECURESTORAGE_CONFIG_DIR).
	SignInDirName = "agent-signin"
	// CredentialFile is the file claude keeps its sign-in in, inside that folder.
	CredentialFile = ".credentials.json"
)

// SignInDirOf is the shared sign-in folder of a helper directory.
func SignInDirOf(helperDir string) string { return filepath.Join(helperDir, SignInDirName) }

// EnsureSignInDir makes the shared sign-in folder (0700) and returns its real path, which is where it is mounted: the
// same path in the sign-in container and in the role VMs. It refuses a folder that can't be mounted or that is itself a link (a link higher up is followed, so the mount is
// the real folder).
func EnsureSignInDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) || badMountPath(dir) {
		return "", errors.New("the sign-in folder can't be mounted (it must be an absolute path without commas or control characters)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s isn't a plain folder; refusing to use it for the sign-in", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || badMountPath(real) {
		return "", errors.New("the sign-in folder can't be mounted")
	}
	return real, nil
}

// SignedInAt says whether the shared folder holds a credential: the file is only stat'ed, never opened.
func SignedInAt(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, CredentialFile))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// LoginEnv is what the sign-in helper's claude needs besides the proxy: the shared folder as its sign-in storage (the
// same path as on the Mac) and a throwaway config folder in the container's memory.
func LoginEnv(signInDir string) map[string]string {
	return map[string]string{"CLAUDE_SECURESTORAGE_CONFIG_DIR": signInDir, "CLAUDE_CONFIG_DIR": "/tmp/claude-login", "HOME": "/tmp"}
}
