package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// The golden table of every command line Rubi Home gives Apple's container tool for an agent VM, taken from the code before
// the VM backend interface was introduced (docs/plans/home-platforms.md, stage P8). macOS behaviour must stay byte-identical,
// so this file's expected value is a file of literal argv lists, not computed from the code. Run with RUBI_UPDATE_GOLDEN=1 only
// to write a new one on purpose.
const goldenFile = "testdata/apple_argv.golden.json"

// recorded runs fn with a container whose commands are recorded and answered as the tool would.
func goldenArgv(t *testing.T) map[string][][]string {
	t.Helper()
	out := map[string][][]string{}
	newApple := func() (*AppleContainer, *[][]string) {
		var cmds [][]string
		a := &AppleContainer{Bin: "container", Image: "rubi-agent-runner:golden", RunnerDir: "/r/runner", EnvDir: t.TempDir(), CPUs: "2", Memory: "2G"}
		a.run = func(_ context.Context, argv []string) ([]byte, error) {
			cmds = append(cmds, append([]string(nil), argv...))
			switch {
			case len(argv) > 2 && argv[1] == "ls":
				return []byte(`[{"configuration":{"id":"rubi-agent-old-` + oldRoleSuffix(goldenRole())[1:] + `"}},{"configuration":{"id":"rubi-login-x"}},{"configuration":{"id":"other"}}]`), nil
			case len(argv) > 1 && argv[1] == "inspect":
				return []byte(`[{"configuration":{"mounts":[{"source":"/s","destination":"/d","options":["readonly"],"type":{"virtiofs":{}}}]}}]`), nil
			case len(argv) > 2 && argv[1] == "image":
				return nil, errors.New("missing")
			case len(argv) > 1 && argv[1] == "run" && containsArg(argv, "--label"):
				return nil, errors.New("stop here") // the sign-in's run; what follows is not a command line
			}
			return nil, nil
		}
		return a, &cmds
	}
	legacy := Project{ID: "my-project", Dir: "/home/alice/my-project"}
	role := goldenRole()
	a, _ := newApple()
	one := func(k string, argv []string) { out[k] = [][]string{argv} }
	one("run_legacy", a.RunArgs(legacy))
	one("run_role", a.RunArgs(role))
	shielded := role
	shielded.EmptyDir = "/home/alice/rubi-home/agent-role-empty"
	shielded.ReadOnly = []ShieldMount{{Name: ".claude", Source: "/h/shield/claude"}, {Name: ".git/hooks", Source: "/h/shield/git_hooks"}}
	one("run_role_shielded", a.RunArgs(shielded))
	one("exec", a.ExecArgs(legacy, "/h/env/e1", []string{"node", runnerMain}))
	one("exec_role_no_envfile", a.ExecArgs(role, "", []string{"id"}))
	one("stop", a.stopArgs(role))
	one("delete", a.deleteArgs(role))
	one("image_inspect", a.imageInspectArgs())
	one("build_args", a.BuildArgs())
	bc, err := a.BuildCommand()
	if err != nil {
		t.Fatal(err)
	}
	one("build_command", bc)
	bare := &AppleContainer{Bin: "container"}
	one("run_defaults", bare.RunArgs(legacy))

	ctx := context.Background()
	rec := func(k string, fn func(a *AppleContainer)) {
		a, cmds := newApple()
		fn(a)
		out[k] = *cmds
	}
	rec("image_exists", func(a *AppleContainer) { a.ImageExists(ctx) })
	rec("ensure_image", func(a *AppleContainer) { _ = a.EnsureImage(ctx) })
	rec("leftovers_role", func(a *AppleContainer) { a.leftoverNames(ctx, role) })
	rec("list", func(a *AppleContainer) { _, _ = a.ListContainers(ctx) })
	rec("inspect_mounts", func(a *AppleContainer) { _, _ = a.InspectMounts(ctx, "c1") })
	rec("remove", func(a *AppleContainer) { _ = a.RemoveContainer(ctx, "c1") })
	rec("run_detached", func(a *AppleContainer) { _ = a.RunDetached(ctx, []string{"container", "run", "-d", "x"}) })
	rec("sweep", func(a *AppleContainer) { a.Sweep(ctx) })
	var signIn string
	rec("start_login", func(a *AppleContainer) {
		a.EnvDir = t.TempDir()
		signIn = filepath.Join(t.TempDir(), "signin")
		_, _ = a.StartLogin(ctx, signIn)
	})
	real, _ := filepath.EvalSymlinks(filepath.Dir(signIn))
	for i, c := range out["start_login"] {
		for j := range c {
			c[j] = loginName.ReplaceAllString(strings.ReplaceAll(c[j], real, "TMP"), "rubi-t3-login-RANDOM")
		}
		out["start_login"][i] = c
	}

	// the start sequence, run for real against the recorder (the pipes and the spawned execs)
	fastPipe(t)
	var cmds [][]string
	a2 := &AppleContainer{Bin: "container", Image: "rubi-agent-runner:golden", RunnerDir: "/r/runner", EnvDir: t.TempDir(), CPUs: "2", Memory: "2G"}
	a2.run = func(_ context.Context, argv []string) ([]byte, error) {
		cmds = append(cmds, append([]string(nil), argv...))
		if len(argv) > 2 && argv[1] == "image" {
			return nil, os.ErrNotExist
		}
		return nil, nil
	}
	vm := newVMPipes(t, func(_ context.Context, argv []string) (Conn, error) { return nopConn{}, nil })
	a2.spawn = func(c context.Context, argv []string) (Conn, error) {
		cmds = append(cmds, append([]string(nil), argv...))
		return vm.spawn(c, argv)
	}
	if _, err := a2.Start(ctx, StartSpec{Project: shielded, Env: map[string]string{"RUBI_RUN_TOKEN": "tok"},
		Pipe: &PipeSpec{Proxy: netpipe.NewListener("p", 1)}}); err != nil {
		t.Fatal(err)
	}
	startLen := len(cmds)
	_ = a2.Stop(ctx)
	out["start_role"] = scrubEnvFile(cmds[:startLen])
	out["stop_sequence"] = cmds[startLen:]
	return out
}

var loginName = regexp.MustCompile(`rubi-t3-login-[0-9a-f]{12}`)

func goldenRole() Project {
	return Project{ID: "my-project", Dir: "/home/alice/my-project", Role: "r_aaaaaaaa", RoleName: "visual-design",
		RoleDir: "/home/alice/rubi-home/agent-roles/r_aaaaaaaa/content", ClaudeDir: "/home/alice/rubi-home/agent-roles/r_aaaaaaaa/claude"}
}

func containsArg(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// scrubEnvFile replaces the random path of the exec's environment file, which is made fresh for every start.
func scrubEnvFile(cmds [][]string) [][]string {
	out := make([][]string, len(cmds))
	for i, c := range cmds {
		c = append([]string(nil), c...)
		for j := range c {
			if j > 0 && c[j-1] == "--env-file" {
				c[j] = "ENVFILE"
			}
		}
		out[i] = c
	}
	return out
}

func TestAppleCommandLinesAreTheGoldenTable(t *testing.T) {
	got := goldenArgv(t)
	if os.Getenv("RUBI_UPDATE_GOLDEN") != "" {
		b, _ := json.MarshalIndent(got, "", " ")
		if err := os.WriteFile(goldenFile, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][][]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s: no longer produced", k)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("%s changed:\n got  %q\n want %q", k, g, w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s is not in the golden table", k)
		}
	}
	for k, w := range want {
		for _, c := range w {
			if strings.Contains(strings.Join(c, " "), "tok") && k != "start_role" {
				t.Errorf("%s: a token on a command line", k)
			}
		}
	}
}
