package home

// The release-version rule (docs/design/rubi-home.md) for the Claude Code hook: this Rubi Home never installs its binary over a
// hook that is newer than it. The hook's version is believed only when the file passes the trust check (here the test seam says
// it does); the same version is a reinstall; an older, insecure, unreadable or development hook is replaced as before.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

func TestInstallHookNeverReplacesANewerHook(t *testing.T) {
	old := version.Version
	version.Version = "v0.14.0"
	t.Cleanup(func() { version.Version = old })
	secure := func(b *HookBinary) { b.Secure = true }
	cases := []struct {
		name, hook string // what the installed hook prints for `version`
		tweak      func(*HookBinary)
		refused    bool
	}{
		{"newer", "echo v0.15.0", secure, true},
		{"newer patch", "echo v0.14.1", secure, true},
		{"older pre-release", "echo v0.14.0-rc1", secure, false},
		{"same", "echo v0.14.0", secure, false},
		{"older", "echo v0.13.0", secure, false},
		{"newer but not trusted", "echo v9.0.0", nil, false}, // a file anyone can write is not believed about its version
		{"unreadable", "exit 3", secure, false},
		{"development build", "echo dev", secure, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := script(t, dir, "src", "echo v0.14.0")
			dst := script(t, dir, "rubi-home", c.hook)
			before, _ := os.ReadFile(dst)
			ran := false
			var out bytes.Buffer
			err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, inspectTweak: c.tweak,
				Run: func(ctx context.Context, p InstallPlan) error { ran = true; return copyRunner(nil)(ctx, p) }})
			if c.refused {
				if err == nil || codeOf(err) != CodeDowngrade || ran {
					t.Fatalf("err %v (code %q), runner called %v", err, codeOf(err), ran)
				}
				if after, _ := os.ReadFile(dst); !bytes.Equal(before, after) {
					t.Fatal("the newer hook was changed")
				}
				return
			}
			if err != nil || !ran {
				t.Fatalf("err %v, runner called %v\n%s", err, ran, out.String())
			}
		})
	}
}

// Through the helper (what the panel's install does): a newer, trusted hook is not replaced, also when the Claude Code plugin
// is the reason to install; the refusal is the downgrade code, and nothing ran as administrator.
func TestPanelHookInstallDoesNotDowngradeTheHook(t *testing.T) {
	h, _ := hookInstallFixture(t) // this Rubi Home is v0.14.0 and the hook counts as root-owned
	if err := os.MkdirAll(filepath.Dir(h.hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	script(t, filepath.Dir(h.hookPath), "rubi-home", "echo v0.15.0")
	runs := 0
	runWith(h, func(ctx context.Context, p InstallPlan) error { runs++; return makeDirAndCopy(ctx, p) })
	if _, err := handle(t, h, "tok", "hook.install", map[string]any{"with_plugin": true}); err != nil {
		t.Fatalf("the request itself: %v", err)
	}
	l := lastOf(t, waitIdle(t, h))
	if runs != 0 || l["result"] != HookResultFailed || l["code"] != CodeDowngrade || !strings.Contains(l["message"].(string), "newer") {
		t.Fatalf("runs %d, last %v", runs, l)
	}
	if b, _ := os.ReadFile(h.hookPath); !strings.Contains(string(b), "v0.15.0") {
		t.Fatal("the newer hook was replaced")
	}
}
