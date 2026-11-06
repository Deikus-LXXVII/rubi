//go:build unix

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/guest"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
)

// The test binary doubles as the network pipe's forwarder in the VM (rubi-netpipe): it says HELLO and serves.
func TestMain(m *testing.M) {
	if os.Getenv("RUBI_TEST_FORWARDER") != "" {
		mux := netpipe.New(stdio{}, netpipe.Config{})
		_ = mux.SendHello()
		_ = mux.Run()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdio) Close() error                { return os.Stdout.Close() }

// fakeVM is a started VM whose guest agent is a guest.Server in this process, running its processes on this computer.
type fakeVM struct {
	t       *testing.T
	srv     *guest.Server
	mu      sync.Mutex
	mounts  []guest.Mount
	execs   []guest.ExecRequest
	powered bool
	stopped bool
	done    chan struct{}
}

func newFakeVM(t *testing.T) *fakeVM {
	f := &fakeVM{t: t, done: make(chan struct{})}
	self, _ := os.Executable()
	f.srv = &guest.Server{
		Mount: func(m guest.Mount) error {
			f.mu.Lock()
			f.mounts = append(f.mounts, m)
			f.mu.Unlock()
			return nil
		},
		PowerOff: func() error {
			f.mu.Lock()
			f.powered = true
			f.mu.Unlock()
			return nil
		},
		Prepare: func(cmd *exec.Cmd, req guest.ExecRequest) {
			f.mu.Lock()
			f.execs = append(f.execs, req)
			f.mu.Unlock()
			cmd.Dir = t.TempDir() // /work is the VM's
			if cmd.Path == pipeBin {
				cmd.Path, cmd.Args = self, []string{self}
				cmd.Env = append(cmd.Env, "RUBI_TEST_FORWARDER=1")
			}
		},
	}
	return f
}

func (f *fakeVM) DialControl() (net.Conn, error) {
	home, vm := net.Pipe()
	go func() { _ = f.srv.Serve(vm) }()
	return home, nil
}
func (f *fakeVM) Stop(time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.stopped {
		f.stopped = true
		close(f.done)
	}
}
func (f *fakeVM) Done() <-chan struct{} { return f.done }
func (f *fakeVM) Err() error            { return nil }
func (f *fakeVM) Console() string       { return "" }

// microRig is a MicroVM over a fakeVM, with a stub `node` on the image's PATH that echoes its stdin and writes its
// environment to a file.
type microRig struct {
	m       *MicroVM
	fake    *fakeVM
	spec    microvm.Spec
	envFile string
}

func newMicroRig(t *testing.T) *microRig {
	bin := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")
	node := "#!/bin/sh\nenv > " + envFile + "\nexec cat\n"
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte(node), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &microRig{fake: newFakeVM(t), envFile: envFile}
	r.m = &MicroVM{QEMU: "/opt/q/bin/qemu-system-x86_64", Arch: microvm.X86_64, Firmware: "/opt/q/qboot.rom",
		Kernel: "/opt/k/vmlinux", Rootfs: "/opt/k/rootfs.img", StateDir: filepath.Join(t.TempDir(), "fresh-home", "vm"),
		ImageEnv: map[string]string{"PATH": bin + ":/usr/bin:/bin", "HOME": "/tmp", "CLAUDE_CONFIG_DIR": "/work/.rubi/claude"}}
	r.m.launch = func(s microvm.Spec) (microVMProcess, error) {
		r.spec = s
		// a fresh Home has no VMs' folder yet: the VM's own folder is made by PrepareDir, inside one that must be there
		if err := microvm.PrepareDir(s.Dir); err != nil {
			return nil, err
		}
		return r.fake, nil
	}
	return r
}

func roleProject(t *testing.T) Project {
	return Project{ID: "web", Dir: "/home/u/src/web", Role: "r_aaaaaa01", RoleName: "reviewer",
		RoleDir: "/home/u/.r/content", ClaudeDir: "/home/u/.r/claude", EmptyDir: "/home/u/.r/empty",
		ReadOnly: []ShieldMount{{Name: ".claude", Source: "/home/u/.r/s1"}}}
}

// A role's VM: its shares are Project.Mounts in order, mounted by the guest as the table says; the pipe's forwarder and
// the runner run as the runner's user in /work with the image's environment under the start's; Stop powers the VM off
// and ends it.
func TestMicroVMStartMountsPipesAndRunsTheRunner(t *testing.T) {
	r := newMicroRig(t)
	p := roleProject(t)
	proxy := netpipe.NewListener("proxy", 4)
	defer proxy.Close()
	conn, err := r.m.Start(context.Background(), StartSpec{Project: p, Env: map[string]string{"RUBI_RUN_TOKEN": "tok", "HOME": "/override"},
		Pipe: &PipeSpec{Proxy: proxy}})
	if err != nil {
		t.Fatal(err)
	}
	if r.m.State() != StateRunning {
		t.Errorf("state %s", r.m.State())
	}
	// the VM's spec: the project's mounts as shares, in order, in its own folder
	var want []microvm.Share
	for _, mt := range p.Mounts() {
		want = append(want, microvm.Share{Source: mt.Source, Target: mt.Target, ReadOnly: mt.ReadOnly})
	}
	if !slices.Equal(r.spec.Shares, want) || r.spec.Dir != filepath.Join(r.m.StateDir, containerName(p)) || r.spec.Name != containerName(p) {
		t.Errorf("spec %+v", r.spec)
	}
	var wantMounts []guest.Mount
	for _, gm := range microvm.GuestMounts(r.spec) {
		wantMounts = append(wantMounts, guest.Mount{Tag: gm.Tag, Target: gm.Target, ReadOnly: gm.ReadOnly})
	}
	r.fake.mu.Lock()
	gotMounts, execs := slices.Clone(r.fake.mounts), slices.Clone(r.fake.execs)
	r.fake.mu.Unlock()
	if !slices.Equal(gotMounts, wantMounts) {
		t.Errorf("mounts %+v", gotMounts)
	}
	if len(execs) != 2 || execs[0].Argv[0] != pipeBin || !slices.Equal(execs[1].Argv, []string{"node", runnerMain}) {
		t.Fatalf("execs %+v", execs)
	}
	for _, e := range execs {
		if e.UID != 10001 || e.GID != 10001 || e.Dir != "/work" {
			t.Errorf("exec %v as %d:%d in %s", e.Argv, e.UID, e.GID, e.Dir)
		}
	}
	// the runner's environment: the image's, the start's on top, and the role's places
	env := execs[1].Env
	if env["RUBI_RUN_TOKEN"] != "tok" || env["HOME"] != "/override" || env["CLAUDE_CONFIG_DIR"] != "/work/.rubi/claude" ||
		env["RUBI_ROLE_DIR"] != RoleMountDir || env["RUBI_CLAUDE_DIR"] != RoleClaudeDir || !strings.Contains(env["PATH"], ":/usr/bin") {
		t.Errorf("runner env %v", env)
	}
	// any other exec in a role VM (a sandbox's command) gets the role's places too, as Apple's container gives every exec
	if c, err := r.m.Exec(context.Background(), []string{"true"}, nil); err != nil {
		t.Fatal(err)
	} else {
		<-c.Done()
	}
	r.fake.mu.Lock()
	last := r.fake.execs[len(r.fake.execs)-1]
	r.fake.mu.Unlock()
	if last.Env["RUBI_ROLE_DIR"] != RoleMountDir || last.Env["RUBI_CLAUDE_DIR"] != RoleClaudeDir || last.Env["HOME"] != "/tmp" {
		t.Errorf("a role VM's exec env %v", last.Env)
	}
	// the runner's stdio is the Conn
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo %q %v", line, err)
	}
	if err := r.m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		t.Error("the runner still runs after Stop")
	}
	// the guest powers off after it has answered (guest.Server), so the fake may note it a moment after Stop returns
	var powered, stopped bool
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r.fake.mu.Lock()
		powered, stopped = r.fake.powered, r.fake.stopped
		r.fake.mu.Unlock()
		if powered || time.Now().After(deadline) {
			break
		}
	}
	if !powered || !stopped || r.m.State() != StateStopped {
		t.Errorf("powered %v stopped %v state %s", powered, stopped, r.m.State())
	}
	if _, err := r.m.Exec(context.Background(), []string{"true"}, nil); err == nil {
		t.Error("exec after Stop")
	}
}

// What can't be run safely is refused before any VM starts, as Apple's backend refuses it.
func TestMicroVMStartRefuses(t *testing.T) {
	for name, mutate := range map[string]func(*StartSpec){
		"no pipe":            func(s *StartSpec) { s.Pipe = nil },
		"comma in a path":    func(s *StartSpec) { s.Project.Dir = "/home/u/a,b" },
		"newline in a path":  func(s *StartSpec) { s.Project.ReadOnly[0].Source = "/x\ny" },
		"role without dirs":  func(s *StartSpec) { s.Project.ClaudeDir = "" },
		"role without empty": func(s *StartSpec) { s.Project.EmptyDir = "" },
	} {
		r := newMicroRig(t)
		launched := false
		r.m.launch = func(microvm.Spec) (microVMProcess, error) { launched = true; return r.fake, nil }
		spec := StartSpec{Project: roleProject(t), Pipe: &PipeSpec{Proxy: netpipe.NewListener("proxy", 4)}}
		spec.Project.ReadOnly = slices.Clone(spec.Project.ReadOnly)
		mutate(&spec)
		if _, err := r.m.Start(context.Background(), spec); err == nil || launched {
			t.Errorf("%s: err %v, launched %v", name, err, launched)
		}
		if r.m.State() != StateFailed {
			t.Errorf("%s: state %s", name, r.m.State())
		}
	}
}

// The runner's stderr reaches the log line by line, never a line that looks secret.
func TestMicroVMStderrLogsLinesButNotSecrets(t *testing.T) {
	var got []string
	w := &stderrLog{}
	old := logPrintf
	logPrintf = func(f string, a ...any) {
		got = append(got, strings.TrimPrefix(fmt.Sprintf(f, a...), "agent runner: "))
	}
	defer func() { logPrintf = old }()
	_, _ = io.WriteString(w, "first li")
	_, _ = io.WriteString(w, "ne\nAuthorization: Bearer sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789\nlast\n")
	_, _ = io.WriteString(w, strings.Repeat("x", stderrLineMax+10)+"\nafter\n")
	if !slices.Equal(got, []string{"first line", "last", "after"}) {
		t.Errorf("logged %q", got)
	}
}
