package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
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
func TestAppleBackendHandsOutItsParts(t *testing.T) {
	b := NewAppleBackend(BackendConfig{Bin: "container", RunnerDir: "/r", EnvDir: "/e", Limits: VMLimits{CPUs: "2", Memory: "2G"}})
	if b.Name() != "apple-container" || !b.Available(context.Background()).OK {
		t.Fatalf("%s", b.Name())
	}
	vm, ok := b.New(Project{ID: "p", Dir: "/p"}).(*AppleContainer)
	if !ok || vm.Bin != "container" || vm.RunnerDir != "/r" || vm.EnvDir != "/e" || vm.CPUs != "2" || vm.Memory != "2G" {
		t.Fatalf("%+v", vm)
	}
	if b.Login() == nil || b.Sessions() == nil || b.Provision() == nil {
		t.Fatal("a part is missing")
	}
	if b.Sessions().ContainerBin() != "container" {
		t.Errorf("the session host's tool is %q", b.Sessions().ContainerBin())
	}
	if !vm.Netless() {
		t.Error("an Apple container VM has a network")
	}
}

// fakeRun answers the commands of the tool from a table, and records them.
type fakeRun struct {
	answers map[string]string
	fail    map[string]bool
	calls   []string
}

func (f *fakeRun) Run(_ context.Context, argv []string) ([]byte, error) {
	j := strings.Join(argv, " ")
	f.calls = append(f.calls, j)
	if f.fail[j] {
		return nil, errors.New("exit 1")
	}
	return []byte(f.answers[j]), nil
}

func (f *fakeRun) Stream(ctx context.Context, argv []string, line func(string)) error {
	out, err := f.Run(ctx, argv)
	for _, l := range strings.Split(string(out), "\n") {
		line(l)
	}
	return err
}

// The Apple provisioner's view of the tool: the version, the service, the image, and the commands that start and build.
func TestAppleProvisionerReadsTheTool(t *testing.T) {
	ctx := context.Background()
	b := NewAppleBackend(BackendConfig{Bin: "container", RunnerDir: "/r"})
	p := b.Provision()
	if ti := p.Titles(); ti.Tool != "Apple container" || ti.Service != "Container service" {
		t.Errorf("titles %+v", ti)
	}
	run := &fakeRun{answers: map[string]string{"container --version": "container CLI version 1.6.1 (build: release)\n"}}
	if v, ok, present := p.Installed(ctx, run); v != "1.6.1" || !ok || !present {
		t.Errorf("installed: %q %v %v", v, ok, present)
	}
	for _, c := range []struct {
		out, want string
		state     string
		action    string
		missing   bool
	}{
		{"container CLI version 1.6.1\n", "1.6.1", StepDone, "", false},
		{"container CLI version 1.5.9\n", "1.6.1", StepTodo, "upgrade", false},
		{"container CLI version 1.7.0\n", "1.6.1", StepDone, "", false},
		{"nonsense\n", "1.6.1", StepFailed, "", false},
		{"", "1.6.1", StepTodo, "install", true},
	} {
		r := &fakeRun{answers: map[string]string{"container --version": c.out}, fail: map[string]bool{"container --version": c.missing}}
		if st := p.Tool(ctx, r, c.want); st.State != c.state || st.Action != c.action {
			t.Errorf("%q: %+v", c.out, st)
		}
	}
	// the commands, exactly
	run = &fakeRun{fail: map[string]bool{"container image inspect " + b.Provision().ImageTag(): true}}
	if st := p.Image(ctx, run); st.State != StepTodo || st.Action != "build" {
		t.Errorf("image: %+v", st)
	}
	var lines []string
	if err := p.StartService(ctx, run, func(l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	prepared, err := p.BuildImage(ctx, run, func(string) {})
	if !prepared || err != nil {
		t.Fatalf("build: %v %v", prepared, err)
	}
	want := []string{"container image inspect " + DefaultImage, "container system start --enable-kernel-install",
		"container build --progress plain -t " + DefaultImage + " -f /r/container/Dockerfile /r"}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("commands %q\nwant %q", run.calls, want)
	}
	// sources that can't be made ready are "not prepared", and the build never starts
	bare := NewAppleBackend(BackendConfig{Bin: "container"}).Provision()
	r2 := &fakeRun{}
	if prepared, err := bare.BuildImage(ctx, r2, func(string) {}); prepared || err == nil || len(r2.calls) != 0 {
		t.Errorf("no directory to build in: %v %v %v", prepared, err, r2.calls)
	}
}

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
