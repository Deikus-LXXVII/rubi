package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
	"github.com/Deikus-LXXVII/rubi/runner"
)

// Only the command lines are tested; no container is ever started.

func TestAppleContainerArgs(t *testing.T) {
	a := &AppleContainer{Bin: "container", RunnerDir: "/opt/rubi/runner", CPUs: "2", Memory: "2G", AuthDir: "/home/alice/rubi-home/agent-auth"}
	p := Project{ID: "my-project", Dir: "/home/alice/my-project"}
	name := containerName(p)
	if !strings.HasPrefix(name, "rubi-agent-my-project-") || containerName(p) != name || containerName(Project{ID: "my-project", Dir: "/other"}) == name {
		t.Fatalf("name %q", name)
	}
	if got := a.BuildArgs(); !reflect.DeepEqual(got, []string{"container", "build", "--progress", "plain", "-t", DefaultImage,
		"-f", "/opt/rubi/runner/container/Dockerfile", "/opt/rubi/runner"}) {
		t.Fatalf("build %v", got)
	}
	run := a.RunArgs(p)
	want := []string{"container", "run", "-d", "--name", name, "--init", "--network", "none", "--user", "10001",
		"--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp", "--mount", "type=bind,source=/home/alice/my-project,target=/work",
		"--workdir", "/work", "--cpus", "2", "--memory", "2G", "--entrypoint", "sleep", DefaultImage, "infinity"}
	if !reflect.DeepEqual(run, want) {
		t.Fatalf("run\n got %v\nwant %v", run, want)
	}
	for _, bad := range []string{"--publish", "-p", "--ssh", "--privileged", "--virtualization", "--cap-add"} {
		for _, x := range run {
			if x == bad {
				t.Errorf("run has %s", bad)
			}
		}
	}
	ex := a.ExecArgs(p, "/tmp/e/env", []string{"node", runnerMain})
	if !reflect.DeepEqual(ex, []string{"container", "exec", "-i", "--user", "10001", "--workdir", "/work", "--env-file", "/tmp/e/env", name, "node", runnerMain}) {
		t.Fatalf("exec %v", ex)
	}
	if got := a.stopArgs(p); !reflect.DeepEqual(got, []string{"container", "stop", "--time", "5", name}) {
		t.Fatalf("stop %v", got)
	}
	if got := a.deleteArgs(p); !reflect.DeepEqual(got, []string{"container", "delete", "--force", name}) {
		t.Fatalf("delete %v", got)
	}
}

// A role VM mounts the project read-write, its own content read-only and its own config directory read-write; no
// sign-in, never --ssh; each role has its own container; the legacy role is as it always was.
func TestAppleContainerRoleArgs(t *testing.T) {
	a := &AppleContainer{Bin: "container", CPUs: "2", Memory: "2G", AuthDir: "/home/alice/rubi-home/agent-auth"}
	legacy := Project{ID: "my-project", Dir: "/home/alice/my-project"}
	mk := func(id, name string) Project {
		return Project{ID: "my-project", Dir: legacy.Dir, Role: id, RoleName: name,
			RoleDir: "/home/alice/rubi-home/agent-roles/" + id + "/content", ClaudeDir: "/home/alice/rubi-home/agent-roles/" + id + "/claude"}
	}
	r1, r2 := mk("r_aaaaaaaa", "visual-design"), mk("r_bbbbbbbb", "reviewer")
	n1, n2 := containerName(r1), containerName(r2)
	if !strings.HasPrefix(n1, "rubi-agent-r-aaaaaaaa-") || !strings.HasPrefix(n2, "rubi-agent-r-bbbbbbbb-") || len(n1) != len("rubi-agent-r-aaaaaaaa-")+16 || n1 == n2 || n1 == containerName(legacy) {
		t.Fatalf("names %q %q %q", n1, n2, containerName(legacy))
	}
	// A rename doesn't change the container.
	renamed := r1
	renamed.RoleName = "something-else"
	if containerName(renamed) != n1 {
		t.Fatal("the name follows the role's name")
	}
	// Same role, another directory: another container.
	other := r1
	other.Dir = "/home/alice/other"
	if containerName(other) == n1 {
		t.Fatal("the name ignores the directory")
	}
	run := a.RunArgs(r1)
	joined := strings.Join(run, " ")
	want := []string{"container", "run", "-d", "--name", n1, "--init", "--network", "none", "--user", "10001",
		"--cap-drop", "ALL", "--read-only", "--tmpfs", "/tmp",
		"--mount", "type=bind,source=/home/alice/my-project,target=/work",
		"--mount", "type=bind,source=/home/alice/rubi-home/agent-roles/r_aaaaaaaa/content,target=/rubi-role,readonly",
		"--mount", "type=bind,source=/home/alice/rubi-home/agent-roles/r_aaaaaaaa/claude,target=/rubi-claude",
		"--env", "RUBI_ROLE_DIR=/rubi-role", "--env", "RUBI_CLAUDE_DIR=/rubi-claude",
		"--workdir", "/work", "--cpus", "2", "--memory", "2G", "--entrypoint", "sleep", DefaultImage, "infinity"}
	if !reflect.DeepEqual(run, want) {
		t.Fatalf("run\n got %v\nwant %v", run, want)
	}
	// The shield: the empty directory over /work/.rubi and the project's .claude, read-only, on top of /work.
	sh := r1
	sh.EmptyDir, sh.ReadOnly = "/home/alice/rubi-home/agent-role-empty", []string{".claude"}
	want0 := "--mount type=bind,source=/home/alice/my-project,target=/work" +
		" --mount type=bind,source=/home/alice/rubi-home/agent-role-empty,target=/work/.rubi,readonly" +
		" --mount type=bind,source=/home/alice/my-project/.claude,target=/work/.claude,readonly" +
		" --mount type=bind,source=/home/alice/rubi-home/agent-roles/r_aaaaaaaa/content"
	if sj := strings.Join(a.RunArgs(sh), " "); !strings.Contains(sj, want0) {
		t.Errorf("shielded run lacks the nested mounts, in order, in %s", sj)
	}
	lsh := legacy
	lsh.EmptyDir, lsh.ReadOnly = sh.EmptyDir, sh.ReadOnly
	if lj := strings.Join(a.RunArgs(lsh), " "); strings.Contains(lj, "/work/.rubi") || strings.Contains(lj, "/work/.claude") {
		t.Errorf("the legacy role is shielded: %s", lj)
	}
	for _, bad := range []string{"--ssh", "--publish", "-p", "--privileged", "--cap-add", "--virtualization"} {
		for _, x := range run {
			if x == bad {
				t.Errorf("role run has %s", bad)
			}
		}
	}
	for _, bad := range []string{"rubi-auth", "agent-auth", "r_bbbbbbbb", "/work/.rubi"} {
		if strings.Contains(joined, bad) {
			t.Errorf("role run mentions %s", bad)
		}
	}
	// The other role's mounts are its own.
	if j2 := strings.Join(a.RunArgs(r2), " "); strings.Contains(j2, "r_aaaaaaaa") || !strings.Contains(j2, "agent-roles/r_bbbbbbbb/content,target=/rubi-role,readonly") {
		t.Fatalf("second role: %s", j2)
	}
	// The legacy role: no role mounts, no role environment.
	lj := strings.Join(a.RunArgs(legacy), " ")
	for _, bad := range []string{"rubi-role", "rubi-claude", "RUBI_ROLE_DIR", "--env"} {
		if strings.Contains(lj, bad) {
			t.Errorf("legacy run has %s", bad)
		}
	}
	// Stop and delete name the role's own container.
	if got := a.stopArgs(r1); got[len(got)-1] != n1 {
		t.Fatalf("stop %v", got)
	}
	// The runner is told it is in role mode through the exec's environment.
	if env := withRoleEnv(map[string]string{"A": "1"}); env["RUBI_ROLE_DIR"] != "/rubi-role" || env["RUBI_CLAUDE_DIR"] != "/rubi-claude" || env["A"] != "1" {
		t.Fatalf("env %v", env)
	}
}

func TestEnvFile(t *testing.T) {
	b, err := EnvFile(map[string]string{"RUBI_RUN_TOKEN": "abc", "A": "1"})
	if err != nil || string(b) != "A=1\nRUBI_RUN_TOKEN=abc\n" {
		t.Fatalf("%q %v", b, err)
	}
	for _, bad := range []map[string]string{{"A": "x\nB=y"}, {"A=B": "x"}, {"": "x"}, {"#c": "x"}} {
		if _, err := EnvFile(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// Start goes through image, delete, run, the forwarder's exec, the runner's exec in that order; there is no network
// command at all; the run token is in the env file and on no command line; the file is gone once the runner is up.
func TestAppleContainerStartSequence(t *testing.T) {
	fastPipe(t)
	var cmds [][]string
	var envPath string
	a := &AppleContainer{RunnerDir: "/opt/rubi/runner", EnvDir: t.TempDir()}
	a.run = func(ctx context.Context, argv []string) ([]byte, error) {
		cmds = append(cmds, argv)
		if len(argv) > 2 && argv[1] == "image" {
			return nil, os.ErrNotExist // image missing
		}
		return nil, nil
	}
	vm := newVMPipes(t, func(ctx context.Context, argv []string) (Conn, error) {
		for i, x := range argv {
			if x == "--env-file" {
				envPath = argv[i+1]
			}
		}
		b, err := os.ReadFile(envPath)
		if err != nil || !strings.Contains(string(b), "RUBI_RUN_TOKEN=tok123") || !strings.Contains(string(b), "HTTPS_PROXY=http://127.0.0.1:3128") {
			t.Errorf("env file at exec time: %q %v", b, err)
		}
		if st, _ := os.Stat(envPath); st.Mode().Perm() != 0o600 {
			t.Errorf("env file mode %v", st.Mode())
		}
		return nopConn{}, nil
	})
	a.spawn = func(ctx context.Context, argv []string) (Conn, error) {
		cmds = append(cmds, argv)
		return vm.spawn(ctx, argv)
	}
	// LookPath is skipped because run is set.
	c, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "my-project", Dir: "/home/alice/my-project"},
		Env:  map[string]string{"RUBI_RUN_TOKEN": "tok123"},
		Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1), Gateway: netpipe.NewListener("g", 1)},
		Prepare: func(ctx context.Context, gateway string) (map[string]string, error) {
			if last := cmds[len(cmds)-1]; last[1] != "exec" {
				t.Errorf("prepared before the pipe ran: %v", last)
			}
			if gateway != "" {
				t.Errorf("gateway %q: a VM without a network has none", gateway)
			}
			return map[string]string{"HTTPS_PROXY": "http://127.0.0.1:3128"}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, c := range cmds {
		seq = append(seq, strings.Join(c[1:min(len(c), 3)], " "))
		if strings.Contains(strings.Join(c, " "), "tok123") {
			t.Errorf("token on a command line: %v", c)
		}
	}
	want := []string{"image inspect", "build --progress", "delete --force", "run -d", "exec -i", "exec -i"}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("sequence %v", seq)
	}
	if !a.Netless() {
		t.Error("an Apple container VM has a network")
	}
	if a.State() != StateRunning {
		t.Fatal(a.State())
	}
	if _, err := os.Stat(envPath); err != nil {
		t.Fatal("env file removed before the runner was up")
	}
	c.(ReadyNotifier).Ready()
	if _, err := os.Stat(envPath); err == nil {
		t.Fatal("env file left after the runner was up")
	}
	cmds = nil
	if err := a.Stop(context.Background()); err != nil || len(cmds) != 2 || a.State() != StateStopped {
		t.Fatalf("stop: %v %v %s", cmds, err, a.State())
	}
}

func TestAppleContainerMissingTool(t *testing.T) {
	a := &AppleContainer{Bin: "/nonexistent/container"}
	if _, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "p", Dir: "/x"}}); err == nil || a.State() != StateFailed {
		t.Fatalf("%v %s", err, a.State())
	}
}

type nopConn struct{}

func (nopConn) Read([]byte) (int, error)    { select {} }
func (nopConn) Write(p []byte) (int, error) { return len(p), nil }
func (nopConn) CloseWrite() error           { return nil }
func (nopConn) Close() error                { return nil }
func (nopConn) Done() <-chan struct{}       { return make(chan struct{}) }

// A failing Prepare stops the container and starts no runner.
func TestAppleContainerPrepareFails(t *testing.T) {
	fastPipe(t)
	a := &AppleContainer{Image: "img"}
	var ran []string
	a.run = func(ctx context.Context, argv []string) ([]byte, error) {
		ran = append(ran, strings.Join(argv[1:min(len(argv), 3)], " "))
		return nil, nil
	}
	a.spawn = newVMPipes(t, func(ctx context.Context, argv []string) (Conn, error) {
		t.Error("runner started")
		return nopConn{}, nil
	}).spawn
	_, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "p", Dir: "/x"}, Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)},
		Prepare: func(context.Context, string) (map[string]string, error) { return nil, errors.New("no proxy") }})
	if err == nil || a.State() != StateFailed || ran[len(ran)-1] != "delete --force" {
		t.Fatalf("%v %s %v", err, a.State(), ran)
	}
}

func TestBuildCommandUsesTheBundledRunner(t *testing.T) {
	env := t.TempDir()
	a := &AppleContainer{EnvDir: env}
	if !strings.HasPrefix(DefaultImage, "rubi-agent-runner:") || len(DefaultImage) != len("rubi-agent-runner:")+12 {
		t.Fatalf("default image %q", DefaultImage)
	}
	argv, err := a.BuildCommand()
	if err != nil {
		t.Fatal(err)
	}
	dir := argv[len(argv)-1]
	if filepath.Dir(dir) != filepath.Join(env, "runner-build") || argv[len(argv)-2] != filepath.Join(dir, "container", "Dockerfile") {
		t.Errorf("build args: %v", argv)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("extracted dir: %v %v", st, err)
	}
	for _, f := range []string{"package.json", "src/main.ts", "container/Dockerfile"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	// A damaged copy is rewritten; an agent_runner_dir override is used as it is.
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.BuildCommand(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "package.json")); string(b) == "{}" {
		t.Error("damaged copy kept")
	}
	over := &AppleContainer{RunnerDir: "/opt/x", EnvDir: env}
	if argv, _ := over.BuildCommand(); argv[len(argv)-1] != "/opt/x" {
		t.Errorf("override: %v", argv)
	}
	if _, err := (&AppleContainer{}).BuildCommand(); err == nil {
		t.Error("no directory to build in should fail")
	}
}

func TestExtractRunnerRewritesAnythingPlanted(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "runner-build", "h")
	bundle := fstest.MapFS{"package.json": {Data: []byte("{}")}, "src/main.ts": {Data: []byte("x")}}
	if _, err := ExtractRunner(dir, bundle); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(){
		"file":    func() { os.WriteFile(filepath.Join(dir, "extra.js"), []byte("evil"), 0o600) },
		"dir":     func() { os.Mkdir(filepath.Join(dir, "node_modules"), 0o700) },
		"symlink": func() { os.Symlink("/etc/passwd", filepath.Join(dir, "link")) },
		"changed": func() { os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("evil"), 0o600) },
	} {
		plant()
		if _, err := ExtractRunner(dir, bundle); err != nil {
			t.Fatal(err)
		}
		if runner.HashFS(os.DirFS(dir)) != runner.HashFS(bundle) {
			t.Errorf("%s was kept", name)
		}
	}
	if ents, _ := os.ReadDir(filepath.Dir(dir)); len(ents) != 1 {
		t.Errorf("temporary directories left: %v", ents)
	}
	// concurrent callers get the same finished directory
	os.RemoveAll(dir)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ExtractRunner(dir, bundle); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if runner.HashFS(os.DirFS(dir)) != runner.HashFS(bundle) {
		t.Error("concurrent extraction left a wrong tree")
	}
}

func TestContainerBinPrefersTheInstalledOne(t *testing.T) {
	old := installedBin
	defer func() { installedBin = old }()
	dir := t.TempDir()
	installedBin = filepath.Join(dir, "container")
	if err := os.WriteFile(installedBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "container"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", other)
	if got := ContainerBin(); got != installedBin {
		t.Errorf("got %q", got)
	}
	installedBin = filepath.Join(dir, "missing")
	if got := ContainerBin(); got != filepath.Join(other, "container") {
		t.Errorf("fallback to PATH: %q", got)
	}
}

// A role's leftovers are removed on start under the current name and under the old-style names, whatever the role
// was called then; other containers are left alone.
func TestAppleContainerRoleLeftovers(t *testing.T) {
	p := Project{ID: "my-project", Dir: "/home/alice/my-project", Role: "r_aaaaaaaa", RoleName: "visual-design"}
	old := "rubi-agent-old-name" + oldRoleSuffix(p)
	other := "rubi-agent-old-name" + oldRoleSuffix(Project{Dir: p.Dir, Role: "r_bbbbbbbb"})
	a := &AppleContainer{}
	a.run = func(ctx context.Context, argv []string) ([]byte, error) {
		if len(argv) > 1 && argv[1] == "ls" {
			return []byte(`[{"configuration":{"id":"` + old + `"}},{"configuration":{"id":"` + other + `"}},{"configuration":{"id":"buildkit"}}]`), nil
		}
		return nil, nil
	}
	got := a.leftoverNames(context.Background(), p)
	if !reflect.DeepEqual(got, []string{containerName(p), old}) {
		t.Fatalf("leftovers %v", got)
	}
	// The legacy role has one name and asks the tool nothing.
	if got := a.leftoverNames(context.Background(), Project{ID: "my-project", Dir: p.Dir}); len(got) != 1 {
		t.Fatalf("legacy leftovers %v", got)
	}
}
