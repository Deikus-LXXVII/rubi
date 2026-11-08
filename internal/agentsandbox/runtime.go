package agentsandbox

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Both files are the complete release input pin, not downloaded at run time.
//
//go:embed runtime/package-lock.json runtime/sources.json
var runtimePins embed.FS

func RuntimeID() string {
	lock, _ := runtimePins.ReadFile("runtime/package-lock.json")
	sources, _ := runtimePins.ReadFile("runtime/sources.json")
	sum := sha256.Sum256(append(lock, sources...))
	return hex.EncodeToString(sum[:])
}
func runtimePlatform() string { return runtime.GOOS + "-" + runtime.GOARCH }
func runtimeRoot(exe string) string {
	return filepath.Join(filepath.Dir(exe), "srt-runtime", RuntimeID(), runtimePlatform())
}
func validateRuntime(root string) error {
	for _, dir := range []string{filepath.Dir(root), root} {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() {
			return errors.New("sandbox: bundled runtime directory missing or unsafe; reinstall Rubi Home")
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "COMPLETE"))
	if err != nil || strings.TrimSpace(string(data)) != RuntimeID() {
		return errors.New("sandbox: bundled runtime is incomplete or has another pin; reinstall Rubi Home")
	}
	files := []string{"bin/node", "bin/srt", "bin/rg", "npm/node_modules/@anthropic-ai/sandbox-runtime/dist/cli.js"}
	if runtime.GOOS == "linux" {
		files = append(files, "bin/socat")
	}
	for _, name := range files {
		st, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !st.Mode().IsRegular() || (strings.HasPrefix(name, "bin/") && st.Mode().Perm()&0111 == 0) {
			return errors.New("sandbox: bundled runtime dependency missing or unsafe; reinstall Rubi Home")
		}
	}
	return nil
}

// ResolveRuntime prefers the adjacent, immutable release bundle. A present but
// incomplete bundle never falls through to a possibly unrelated system runtime.
// Source checkouts can still use explicitly configured dependencies.
func ResolveRuntime(l Launch, exe string) (Launch, error) {
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return l, errors.New("sandbox: launcher installation path cannot be resolved")
	}
	exe = real
	base := filepath.Join(filepath.Dir(exe), "srt-runtime")
	st, err := os.Lstat(base)
	if os.IsNotExist(err) {
		if l.SRT == "" {
			return l, errors.New("sandbox: bundled runtime missing; reinstall Rubi Home or explicitly provision srt")
		}
		return l, nil
	}
	if err != nil || !st.IsDir() {
		return l, errors.New("sandbox: unsafe bundled runtime directory")
	}
	root := runtimeRoot(exe)
	if err := validateRuntime(root); err != nil {
		return l, err
	}
	l.SRT = filepath.Join(root, "bin", "srt")
	bin := filepath.Join(root, "bin")
	dirs := []string{bin}
	for _, d := range l.SearchPath {
		if d != bin {
			dirs = append(dirs, d)
		}
	}
	if len(l.SearchPath) == 0 {
		dirs = append(dirs, "/usr/bin", "/bin")
	}
	l.SearchPath = dirs
	found := false
	for _, p := range l.Policy.RuntimeRead {
		if p == root {
			found = true
		}
	}
	if !found {
		l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, root)
	}
	return l, nil
}
