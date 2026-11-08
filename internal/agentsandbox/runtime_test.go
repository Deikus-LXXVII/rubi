package agentsandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runtimeFixture(t *testing.T) (exe, root string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("release sandbox runtimes are Unix-only")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(base, "rubi-home")
	if err := os.WriteFile(exe, []byte("fake home"), 0755); err != nil {
		t.Fatal(err)
	}
	root = runtimeRoot(exe)
	for _, name := range []string{"bin/node", "bin/rg", "bin/srt", "bin/socat", "npm/node_modules/@anthropic-ai/sandbox-runtime/dist/cli.js", "COMPLETE"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		data := "#!/bin/sh\nexit 0\n"
		if name == "COMPLETE" {
			data = RuntimeID() + "\n"
		}
		if err := os.WriteFile(path, []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "rubi-agent-exec"), []byte("fake relay"), 0755); err != nil {
		t.Fatal(err)
	}
	writeRuntimeManifest(t, root)
	return exe, root
}
func writeRuntimeManifest(t *testing.T, root string) {
	t.Helper()
	var manifest strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if name == "COMPLETE" || name == "SHA256SUMS" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(&manifest, "%x  %s\n", sha256.Sum256(data), filepath.ToSlash(name))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(manifest.String()), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestBundledRuntimeIsPreferredAndProtected(t *testing.T) {
	exe, root := runtimeFixture(t)
	p, settings := fixture(t)
	l, err := ResolveRuntime(Launch{Policy: p, SRT: "/not-the-bundle", SearchPath: []string{"/usr/bin"}}, exe)
	if err != nil {
		t.Fatal(err)
	}
	if l.SRT != filepath.Join(root, "bin/srt") || l.SearchPath[0] != filepath.Join(root, "bin") || !contains(l.Policy.RuntimeRead, root) {
		t.Fatal("bundled dependencies not selected")
	}
	if _, err := Build(l.Policy, []string{settings}); err != nil {
		t.Fatal(err)
	}
	l.Policy.Projects = []string{filepath.Dir(root)}
	if _, err := Build(l.Policy, []string{settings}); err == nil {
		t.Fatal("agent could write into its runtime")
	}
}
func TestBundledRuntimeMissingOrUnsafeNeverFallsBack(t *testing.T) {
	for _, name := range []string{"COMPLETE", "bin/node", "bin/srt", "bin/rg", "npm/node_modules/@anthropic-ai/sandbox-runtime/dist/cli.js", "pin", "symlink-root", "symlink-node"} {
		t.Run(name, func(t *testing.T) {
			exe, root := runtimeFixture(t)
			switch name {
			case "pin":
				if err := os.WriteFile(filepath.Join(root, "COMPLETE"), []byte("wrong pin"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-root":
				moved := root + "-moved"
				if err := os.Rename(root, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, root); err != nil {
					t.Skip(err)
				}
			case "symlink-node":
				node := filepath.Join(root, "bin/node")
				if err := os.Remove(node); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "bin/rg"), node); err != nil {
					t.Skip(err)
				}
			default:
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ResolveRuntime(Launch{SRT: "/system/srt"}, exe); err == nil {
				t.Fatal("unsafe bundle fell back to system srt")
			}
		})
	}
	if runtime.GOOS == "linux" {
		t.Run("socat", func(t *testing.T) {
			exe, root := runtimeFixture(t)
			if err := os.Remove(filepath.Join(root, "bin/socat")); err != nil {
				t.Fatal(err)
			}
			if _, err := ResolveRuntime(Launch{SRT: "/system/srt"}, exe); err == nil {
				t.Fatal("missing socat accepted")
			}
		})
	}
}
func TestNoBundleNeedsExplicitRuntime(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "rubi-home")
	if err := os.WriteFile(exe, []byte("development binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRuntime(Launch{}, exe); err == nil || !strings.Contains(err.Error(), "reinstall") {
		t.Fatal(err)
	}
	l, err := ResolveRuntime(Launch{SRT: "/explicit/srt", SearchPath: []string{"/explicit/bin"}}, exe)
	if err != nil || l.SRT != "/explicit/srt" || l.SearchPath[0] != "/explicit/bin" {
		t.Fatal("explicit development runtime lost", err)
	}
}
func TestRuntimeInstallCopiesImmutableCompanions(t *testing.T) {
	exe, root := runtimeFixture(t)
	dst := filepath.Join(t.TempDir(), "install")
	for i := 0; i < 2; i++ {
		if err := InstallRuntimeCompanions(filepath.Dir(exe), dst); err != nil {
			t.Fatal(err)
		}
	}
	installed := runtimeRoot(filepath.Join(dst, "rubi-home"))
	if err := VerifyRuntimeFiles(installed); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(root, "bin/node"))
	b, err := os.ReadFile(filepath.Join(installed, "bin/node"))
	if err != nil || string(a) != string(b) {
		t.Fatal("runtime not copied", err)
	}
	relay, err := os.ReadFile(filepath.Join(dst, "rubi-agent-exec"))
	if err != nil || string(relay) != "fake relay" {
		t.Fatal("launcher missing", err)
	}
	if err := os.Remove(filepath.Join(installed, "COMPLETE")); err != nil {
		t.Fatal(err)
	}
	if err := InstallRuntimeCompanions(filepath.Dir(exe), dst); err == nil {
		t.Fatal("incomplete installed runtime reused")
	}
}
func TestRuntimeInstallRefusesTamperAndSymlinks(t *testing.T) {
	for _, scenario := range []string{"checksum", "unmanifested", "traversal", "symlink-file", "symlink-target", "missing-launcher"} {
		t.Run(scenario, func(t *testing.T) {
			exe, root := runtimeFixture(t)
			dst := filepath.Join(t.TempDir(), "install")
			switch scenario {
			case "checksum":
				if err := os.WriteFile(filepath.Join(root, "bin/node"), []byte("tampered"), 0755); err != nil {
					t.Fatal(err)
				}
			case "unmanifested":
				if err := os.WriteFile(filepath.Join(root, "extra"), []byte("extra"), 0600); err != nil {
					t.Fatal(err)
				}
			case "traversal":
				if err := os.WriteFile(filepath.Join(root, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  ../outside\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-file":
				node := filepath.Join(root, "bin/node")
				if err := os.Remove(node); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "bin/rg"), node); err != nil {
					t.Skip(err)
				}
			case "symlink-target":
				target := runtimeRoot(filepath.Join(dst, "rubi-home"))
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, target); err != nil {
					t.Skip(err)
				}
			case "missing-launcher":
				if err := os.Remove(filepath.Join(filepath.Dir(exe), "rubi-agent-exec")); err != nil {
					t.Fatal(err)
				}
			}
			if err := InstallRuntimeCompanions(filepath.Dir(exe), dst); err == nil {
				t.Fatal("unsafe release installed")
			}
		})
	}
}
func TestRuntimeInstallLegacyDevelopmentBundleIsNoop(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := InstallRuntimeCompanions(src, dst); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil || len(entries) != 0 {
		t.Fatal("legacy install changed", err)
	}
}
func TestSystemBubblewrapMissingIsActionable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux dependency")
	}
	p, settings := fixture(t)
	dir := p.RuntimeRead[0]
	for _, name := range []string{"node", "rg", "srt", "socat", "provider"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Run(context.Background(), Launch{Policy: p, SettingsDir: settings, SRT: filepath.Join(dir, "srt"), CLI: filepath.Join(dir, "provider"), SearchPath: []string{dir}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "system bubblewrap") || !strings.Contains(err.Error(), "no unconfined fallback") {
		t.Fatal(err)
	}
	files, err := os.ReadDir(settings)
	if err != nil || len(files) != 0 {
		t.Fatal("failed preflight wrote settings")
	}
}

func TestRuntimeResolutionFollowsTheExecutableInstallNotItsAlias(t *testing.T) {
	exe, root := runtimeFixture(t)
	alias := filepath.Join(t.TempDir(), "rubi-home")
	if err := os.Symlink(exe, alias); err != nil {
		t.Skip(err)
	}
	l, err := ResolveRuntime(Launch{}, alias)
	if err != nil || l.SRT != filepath.Join(root, "bin/srt") {
		t.Fatal("runtime selected beside alias instead of real install", err)
	}
}
func TestRuntimeInstallRefusesSymlinkedSourceTree(t *testing.T) {
	exe, _ := runtimeFixture(t)
	base := filepath.Join(filepath.Dir(exe), "srt-runtime")
	moved := base + "-moved"
	if err := os.Rename(base, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, base); err != nil {
		t.Skip(err)
	}
	if err := InstallRuntimeCompanions(filepath.Dir(exe), t.TempDir()); err == nil {
		t.Fatal("symlinked source tree installed")
	}
}
