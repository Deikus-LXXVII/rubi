package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// The shared folders of a VM, in the order they are applied: a later one sits on top of an earlier. This is the one list a
// backend turns into its own mounts (bind mounts for Apple's tool, shares for a microvm); the order and the read-only flags
// are what the shield depends on.
func TestProjectMountsAreTheSharedFoldersInOrder(t *testing.T) {
	legacy := Project{ID: "p", Dir: "/home/a/p"}
	if got := legacy.Mounts(); !reflect.DeepEqual(got, []Mount{{Source: "/home/a/p", Target: "/work"}}) {
		t.Fatalf("legacy: %+v", got)
	}
	// the legacy role has no role mounts even if shield fields are set
	legacy.EmptyDir = "/h/empty"
	if got := legacy.Mounts(); len(got) != 1 {
		t.Fatalf("legacy with a shield: %+v", got)
	}
	role := Project{ID: "p", Dir: "/home/a/p", Role: "r_1", RoleDir: "/h/roles/r_1/content", ClaudeDir: "/h/roles/r_1/claude",
		EmptyDir: "/h/empty", ReadOnly: []ShieldMount{{Name: ".claude", Source: "/h/s/claude"}, {Name: ".git/hooks", Source: "/h/s/hooks"}}}
	want := []Mount{
		{"/home/a/p", "/work", false},
		{"/h/empty", "/work/.rubi", true},
		{"/h/s/claude", "/work/.claude", true},
		{"/h/s/hooks", "/work/.git/hooks", true},
		{"/h/roles/r_1/content", RoleMountDir, true},
		{"/h/roles/r_1/claude", RoleClaudeDir, false},
	}
	if got := role.Mounts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("role:\n got %+v\nwant %+v", got, want)
	}
	// only the project and the role's config directory are writable
	for _, m := range role.Mounts() {
		rw := m.Target == "/work" || m.Target == RoleClaudeDir
		if m.ReadOnly == rw {
			t.Errorf("%s: read-only is %v", m.Target, m.ReadOnly)
		}
	}
}

// No backend: not available, with the reason; nothing it makes can start anything, and it has no parts to hand out.
func TestNoBackendIsUnavailableAndStartsNothing(t *testing.T) {
	b := NoBackend("agents don't run on Linux yet")
	if b.Name() != "none" {
		t.Errorf("name %q", b.Name())
	}
	av := b.Available(context.Background())
	if av.OK || av.Reason != "agents don't run on Linux yet" {
		t.Fatalf("%+v", av)
	}
	if b.Login() != nil || b.Sessions() != nil || b.Provision() != nil {
		t.Error("a backend that isn't there hands out parts")
	}
	b.Sweep(context.Background()) // does nothing, does not crash
	vm := b.New(Project{ID: "p", Dir: "/p"})
	if vm.State() != StateStopped {
		t.Errorf("state %s", vm.State())
	}
	if c, err := vm.Start(context.Background(), StartSpec{}); c != nil || !errors.Is(err, errNoBackend) {
		t.Errorf("start: %v %v", c, err)
	}
	if c, err := vm.Exec(context.Background(), []string{"id"}, nil); c != nil || !errors.Is(err, errNoBackend) {
		t.Errorf("exec: %v %v", c, err)
	}
	if err := vm.Stop(context.Background()); err != nil {
		t.Errorf("stop: %v", err)
	}
}

// The Apple backend hands out the Apple container's parts with the configuration it was made from.
func TestVersionComparison(t *testing.T) {
	for _, c := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{{"1.6.1", "1.6.1", 0, true}, {"1.5.9", "1.6.1", -1, true}, {"2.0.0", "1.9.9", 1, true}, {"x", "1.6.1", -1, false}} {
		if cmp, ok := CompareVersions(c.a, c.b); cmp != c.cmp || ok != c.ok {
			t.Errorf("%s %s: %d %v", c.a, c.b, cmp, ok)
		}
	}
	if NormalizeVersion("1.6.1") != "1.6.1" || NormalizeVersion("junk") != "0.0.0" {
		t.Error("NormalizeVersion")
	}
}
