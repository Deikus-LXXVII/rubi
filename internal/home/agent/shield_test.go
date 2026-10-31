package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func treeHas(dir, needle string) bool {
	found := false
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), needle) {
				found = true
			}
		}
		return nil
	})
	return found
}

// Opening the helper (agent.New) never removes a running Home's snapshots; only SweepShields does.
func TestNewDoesNotSweepShields(t *testing.T) {
	dir := t.TempDir()
	snap := filepath.Join(dir, RoleShieldDir, "k-r_a", "claude")
	write(t, filepath.Join(snap, "x"), "1")
	m := New(Options{Dir: dir})
	defer m.Close()
	if _, err := os.Stat(filepath.Join(snap, "x")); err != nil {
		t.Fatalf("New removed a live snapshot: %v", err)
	}
	m.SweepShields()
	if _, err := os.Stat(snap); err == nil {
		t.Fatal("SweepShields left it")
	}
}

// A directory swapped for a symlink between the listing and the opening leaks nothing from outside the project.
func TestSnapshotSwapLeaksNothing(t *testing.T) {
	cases := []struct {
		name, rel, at string // the seam fires when the directory at (project-relative) is listed
		swap          func(proj, outside string)
	}{
		{"subdir", ".claude", ".claude", func(p, o string) {
			_ = os.RemoveAll(filepath.Join(p, ".claude", "sub"))
			_ = os.Symlink(o, filepath.Join(p, ".claude", "sub"))
		}},
		{"top", ".claude", ".claude", func(p, o string) {
			_ = os.Rename(filepath.Join(p, ".claude"), filepath.Join(p, ".claude.moved"))
			_ = os.Symlink(o, filepath.Join(p, ".claude"))
		}},
		{"git-parent", ".git/hooks", ".git/hooks", func(p, o string) {
			_ = os.Rename(filepath.Join(p, ".git"), filepath.Join(p, ".git.moved"))
			_ = os.Symlink(o, filepath.Join(p, ".git"))
		}},
		{"hooks-subdir", ".git/hooks", ".git/hooks", func(p, o string) {
			_ = os.RemoveAll(filepath.Join(p, ".git", "hooks", "sub"))
			_ = os.Symlink(o, filepath.Join(p, ".git", "hooks", "sub"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			proj, outside := filepath.Join(root, "p"), filepath.Join(root, "outside")
			for _, f := range []string{"id_rsa", "sub/id_rsa", "hooks/id_rsa", "hooks/sub/id_rsa", "sub/x"} {
				write(t, filepath.Join(outside, f), "SECRET-KEY")
			}
			write(t, filepath.Join(proj, filepath.FromSlash(tc.rel), "keep"), "ok")
			write(t, filepath.Join(proj, filepath.FromSlash(tc.rel), "sub", "f"), "ok")
			fired := false
			shieldSeam = func(rel string) {
				if rel == tc.at && !fired {
					fired = true
					tc.swap(proj, outside)
				}
			}
			defer func() { shieldSeam = nil }()
			dst := filepath.Join(root, "snap")
			if _, err := snapshotDir(proj, tc.rel, dst); err != nil {
				t.Fatal(err)
			}
			if !fired {
				t.Fatal("the seam never fired")
			}
			if treeHas(dst, "SECRET-KEY") {
				t.Fatal("a file from outside the project reached the snapshot")
			}
		})
	}
}

func TestFingerprintHashesWholeFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	big := strings.Repeat("a", 5<<20)
	write(t, p, big+"x")
	a := fingerprint(p)
	write(t, p, big+"y")
	if fingerprint(p) == a {
		t.Fatal("a change past 4 MiB went unseen")
	}
}

func TestGuardWorktreeAndExtraFiles(t *testing.T) {
	root := t.TempDir()
	proj, gd := filepath.Join(root, "wt"), filepath.Join(root, "main", ".git", "worktrees", "wt")
	common := filepath.Join(root, "main", ".git")
	write(t, filepath.Join(common, "HEAD"), "ref: x\n")
	write(t, filepath.Join(common, "config"), "[core]\n")
	write(t, filepath.Join(gd, "HEAD"), "ref: x\n")
	write(t, filepath.Join(gd, "commondir"), "../..\n")
	write(t, filepath.Join(proj, ".git"), "gitdir: "+gd+"\n")
	g := newFileGuard(proj)
	if len(g.changed(proj)) != 0 {
		t.Fatal("changed at start")
	}
	write(t, filepath.Join(common, "config"), "[core]\nhooksPath=/x\n")
	write(t, filepath.Join(proj, ".envrc"), "x")
	write(t, filepath.Join(proj, ".vscode", "tasks.json"), "{}")
	ch := strings.Join(g.changed(proj), ",")
	for _, w := range []string{"gitdir:common-config", ".envrc", ".vscode/tasks.json"} {
		if !strings.Contains(ch, w) {
			t.Errorf("%s unguarded: %s", w, ch)
		}
	}
	// A .git file pointing at a random place (no HEAD, outside the project) is not followed.
	write(t, filepath.Join(proj, ".git"), "gitdir: "+filepath.Join(root, "nowhere")+"\n")
	for n := range newFileGuard(proj).paths {
		if strings.HasPrefix(n, "gitdir:") {
			t.Fatalf("followed %s", n)
		}
	}
}

func TestGuardFreshReportsOncePerChange(t *testing.T) {
	proj := t.TempDir()
	g := newFileGuard(proj)
	write(t, filepath.Join(proj, ".mcp.json"), "1")
	if ch := g.fresh(); len(ch) != 1 {
		t.Fatalf("first: %v", ch)
	}
	if ch := g.fresh(); len(ch) != 0 {
		t.Fatalf("repeated: %v", ch)
	}
	write(t, filepath.Join(proj, ".mcp.json"), "2")
	if ch := g.fresh(); len(ch) != 1 {
		t.Fatalf("second change: %v", ch)
	}
}

func TestSnapshotFailureIsReported(t *testing.T) {
	root := t.TempDir()
	proj, empty := filepath.Join(root, "p"), filepath.Join(root, "home", RoleEmptyDir)
	write(t, filepath.Join(proj, ".claude", "a"), strings.Repeat("a", snapshotMaxBytes+1))
	s := roleShield(proj, empty, "r_a")
	if s.EmptyDir == "" || len(s.snapFailed) != 1 || s.snapFailed[0] != ".claude" {
		t.Fatalf("not reported: %+v", s)
	}
}

func drainEvents(t *testing.T, m *Manager, kind string, wait time.Duration) int {
	t.Helper()
	n := 0
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		items, _ := m.Next(context.Background(), 50*time.Millisecond)
		for _, it := range items {
			if it.Type == "event" && strings.Contains(string(it.Event), `"`+kind+`"`) {
				n++
			}
		}
	}
	return n
}

// A change made while the VM idles is noticed by the timer, once, not only when the runner exits.
func TestGuardNoticesAChangeWhileTheVMRuns(t *testing.T) {
	old := guardInterval.Swap(int64(20 * time.Millisecond))
	defer guardInterval.Store(old)
	re := newRoleEnv(t, setLimits(1, 4))
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x")
	eventually(t, "started", func() bool { return re.running() == 1 })
	write(t, filepath.Join(re.dir, "..", "my-project", ".mcp.json"), `{"mcpServers":{}}`)
	if n := drainEvents(t, re.m, "project_files_changed", time.Second); n != 1 {
		t.Fatalf("events: %d", n)
	}
}

// A start that fails after the snapshots were made leaves none behind.
func TestFailedStartRemovesSnapshots(t *testing.T) {
	re := newRoleEnv(t, setLimits(1, 4), func(o *Options) {
		o.NewVM = func(Project) VM { return &Fake{StartErr: errors.New("boom")} }
	})
	a, _ := re.put("role-a", nil)
	_, _ = re.op("start", map[string]any{"project": "my-project", "role": a, "prompt": "x"})
	time.Sleep(300 * time.Millisecond)
	if ents, _ := os.ReadDir(filepath.Join(re.dir, RoleShieldDir)); len(ents) != 0 {
		t.Fatalf("snapshots left: %v", ents)
	}
}
