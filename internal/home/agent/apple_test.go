package agent

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Only the command lines are tested; no container is ever started.

func TestAppleContainerArgs(t *testing.T) {
	a := &AppleContainer{RunnerDir: "/opt/rubi/runner", CPUs: "2", Memory: "2G"}
	p := Project{ID: "my-project", Dir: "/home/alice/my-project"}
	name := containerName(p)
	if !strings.HasPrefix(name, "rubi-agent-my-project-") || containerName(p) != name || containerName(Project{ID: "my-project", Dir: "/other"}) == name {
		t.Fatalf("name %q", name)
	}
	if got := a.BuildArgs(); !reflect.DeepEqual(got, []string{"container", "build", "--progress", "plain", "-t", DefaultImage,
		"-f", "/opt/rubi/runner/container/Dockerfile", "/opt/rubi/runner"}) {
		t.Fatalf("build %v", got)
	}
	if got := a.networkCreateArgs(); !reflect.DeepEqual(got, []string{"container", "network", "create", "--internal", DefaultNetwork}) {
		t.Fatalf("network %v", got)
	}
	run := a.RunArgs(p)
	want := []string{"container", "run", "-d", "--name", name, "--init", "--network", DefaultNetwork, "--user", "10001",
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

// Start goes through inspect, network, delete, run, exec in that order; the run token is in the env
// file and on no command line; the file is gone once the runner is up.
func TestAppleContainerStartSequence(t *testing.T) {
	var cmds [][]string
	var envPath string
	netMade := false
	a := &AppleContainer{RunnerDir: "/opt/rubi/runner", EnvDir: t.TempDir()}
	a.run = func(ctx context.Context, argv []string) ([]byte, error) {
		cmds = append(cmds, argv)
		if len(argv) > 2 && argv[1] == "image" || len(argv) > 2 && argv[1] == "network" && argv[2] == "inspect" && !netMade {
			return nil, os.ErrNotExist // image and network missing
		}
		if len(argv) > 2 && argv[1] == "network" && argv[2] == "create" {
			netMade = true
		}
		if len(argv) > 2 && argv[1] == "network" && argv[2] == "inspect" {
			return []byte(`[{"id":"rubi-agent","config":{"Internal":true}}]`), nil
		}
		return nil, nil
	}
	a.spawn = func(ctx context.Context, argv []string) (Conn, error) {
		cmds = append(cmds, argv)
		for i, x := range argv {
			if x == "--env-file" {
				envPath = argv[i+1]
			}
		}
		b, err := os.ReadFile(envPath)
		if err != nil || !strings.Contains(string(b), "RUBI_RUN_TOKEN=tok123") {
			t.Errorf("env file at exec time: %q %v", b, err)
		}
		if st, _ := os.Stat(envPath); st.Mode().Perm() != 0o600 {
			t.Errorf("env file mode %v", st.Mode())
		}
		return nopConn{}, nil
	}
	// LookPath is skipped because run is set.
	c, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "my-project", Dir: "/home/alice/my-project"},
		Env: map[string]string{"RUBI_RUN_TOKEN": "tok123", "HTTPS_PROXY": "http://192.0.2.1:3128"}})
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
	want := []string{"image inspect", "build --progress", "network inspect", "network create", "network inspect", "delete --force", "run -d", "exec -i"}
	if !reflect.DeepEqual(seq, want) {
		t.Fatalf("sequence %v", seq)
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

func TestAppleContainerFailsClosedOnNetwork(t *testing.T) {
	for name, out := range map[string]string{"not internal": `[{"id":"x","internal":false}]`, "no field": `[{"id":"x"}]`, "not json": `oops`, "": ``} {
		a := &AppleContainer{Image: "img"}
		var ran []string
		a.run = func(ctx context.Context, argv []string) ([]byte, error) {
			ran = append(ran, strings.Join(argv[1:min(len(argv), 3)], " "))
			if argv[1] == "network" && argv[2] == "inspect" {
				return []byte(out), nil
			}
			return nil, nil
		}
		a.spawn = func(ctx context.Context, argv []string) (Conn, error) {
			t.Error("runner started")
			return nopConn{}, nil
		}
		if _, err := a.Start(context.Background(), StartSpec{Project: Project{ID: "p", Dir: "/x"}}); err == nil || a.State() != StateFailed {
			t.Errorf("%s: started (%v)", name, err)
		}
		for _, r := range ran {
			if strings.HasPrefix(r, "run ") {
				t.Errorf("%s: a container was run", name)
			}
		}
	}
	if !findInternal(map[string]any{"a": []any{map[string]any{"Config": map[string]any{"INTERNAL": true}}}}) {
		t.Error("nested internal not found")
	}
}
