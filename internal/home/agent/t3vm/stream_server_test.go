package t3vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// fakeStreamHost stands in for the microVM backend's session host.
type fakeStreamHost struct {
	mu       sync.Mutex
	readyErr error
	startErr error
	execErr  error // given to every VM it starts
	vms      []*fakeSessionVM
}

func (h *fakeStreamHost) Ready() error { h.mu.Lock(); defer h.mu.Unlock(); return h.readyErr }
func (h *fakeStreamHost) StartSessionVM(ctx context.Context, name string, mounts []agent.Mount) (agent.SessionVM, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.startErr != nil {
		return nil, h.startErr
	}
	vm := &fakeSessionVM{name: name, mounts: mounts, code: 5, out: "VERSION 9.9\n", execErr: h.execErr}
	h.vms = append(h.vms, vm)
	return vm, nil
}
func (h *fakeStreamHost) all() []*fakeSessionVM {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.vms)
}

type fakeSessionVM struct {
	mu      sync.Mutex
	name    string
	mounts  []agent.Mount
	pipes   int
	stopped bool
	execs   []fakeExec
	code    int
	out     string
	execErr error
}

type fakeExec struct {
	dir  string
	env  map[string]string
	argv []string
}

func (v *fakeSessionVM) StartPipe(ctx context.Context, spec *agent.PipeSpec, env map[string]string, giveUp func()) (func(), error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if spec == nil || spec.Proxy == nil || spec.Hook == nil || spec.T3 == nil {
		return nil, errors.New("a pipe without its services")
	}
	v.pipes++
	return func() {}, nil
}

// ExecIn: claude --version prints out; anything else echoes stdin upper-cased and ends with code.
func (v *fakeSessionVM) ExecIn(ctx context.Context, dir string, env map[string]string, argv []string, stderr io.Writer) (agent.SessionProc, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.execErr != nil {
		return nil, v.execErr
	}
	v.execs = append(v.execs, fakeExec{dir, env, argv})
	if stderr == nil {
		stderr = io.Discard
	}
	if slices.Contains(argv, "--version") {
		return &versionProc{out: strings.NewReader(v.out)}, nil
	}
	return sessionProc{newFakeProc(v.code, stderr)}, nil
}
func (v *fakeSessionVM) Stop(context.Context) error {
	v.mu.Lock()
	v.stopped = true
	v.mu.Unlock()
	return nil
}
func (v *fakeSessionVM) snap() (stopped bool, execs []fakeExec) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stopped, slices.Clone(v.execs)
}

type sessionProc struct{ *fakeProc }

func (sessionProc) Done() <-chan struct{} { return make(chan struct{}) }

// versionProc prints out and ends with 0.
type versionProc struct{ out io.Reader }

func (p *versionProc) Read(b []byte) (int, error) { return p.out.Read(b) }
func (*versionProc) Write(b []byte) (int, error)  { return len(b), nil }
func (*versionProc) CloseWrite() error            { return nil }
func (*versionProc) Close() error                 { return nil }
func (*versionProc) Wait() (int, error)           { return 0, nil }
func (*versionProc) Done() <-chan struct{}        { return make(chan struct{}) }

func streamWorld(t *testing.T) (*world, *fakeStreamHost) {
	h := &fakeStreamHost{}
	w := newWorld(t, func(o *Options) { o.Host, o.Stream, o.StreamClaude = nil, h, "/opt/sdk/claude" })
	return w, h
}

func (w *world) runShim(stdin string, env []string, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	o := ClientOptions{ExecTool: "container", Stdin: strings.NewReader(stdin),
		Getwd:   func() (string, error) { return w.cwd, nil },
		Environ: func() []string { return env },
		Exec: func(string, []string, []string, *os.File) error {
			w.t.Error("a streamed session ran a tool")
			return errors.New("no")
		}}
	code := Run(w.sock, testRole, args, &stdout, &stderr, o)
	return code, stdout.String(), stderr.String()
}

func TestStreamedSessionRunsClaudeInItsVM(t *testing.T) {
	w, h := streamWorld(t)
	code, out, errOut := w.runShim("hi", []string{"ANTHROPIC_MODEL=m", "GITHUB_TOKEN=secret"}, w.argv()...)
	if code != 5 || out != "HI" || errOut != "warn" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	vms := h.all()
	if len(vms) != 1 {
		t.Fatalf("%d VMs", len(vms))
	}
	vm := vms[0]
	eventually(t, "the VM stopped", func() bool { s, _ := vm.snap(); return s })
	if !strings.HasPrefix(vm.name, ContainerPrefix) || vm.pipes != 1 {
		t.Errorf("name %q, pipes %d", vm.name, vm.pipes)
	}
	// the folders: the workspace read-write at its own path, the role's, the policy folders read-only, nothing else
	want := map[string]bool{w.cwd: false, w.be.role.ClaudeDir: false, w.be.role.ContentDir: true, w.att: true,
		filepath.Join(w.dir, "agent-signin"): false, "/etc/claude-code": true, "/etc/rubi": true}
	got := map[string]bool{}
	for _, m := range vm.mounts {
		got[m.Target] = m.ReadOnly
		if m.Target == "/etc/claude-code" || m.Target == "/etc/rubi" {
			if !strings.HasPrefix(m.Source, filepath.Join(w.dir, "t3vm", "vm-etc")) {
				t.Errorf("policy folder %s from %s", m.Target, m.Source)
			}
			if ents, _ := os.ReadDir(m.Source); len(ents) != 1 {
				t.Errorf("policy folder %s holds %d files", m.Source, len(ents))
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}
	for k, ro := range want {
		if g, ok := got[k]; !ok || g != ro {
			t.Errorf("mount %s: present %v read-only %v, want read-only %v", k, ok, g, ro)
		}
	}
	_, execs := vm.snap()
	if len(execs) != 1 {
		t.Fatalf("%d execs", len(execs))
	}
	e := execs[0]
	if e.dir != w.cwd || e.argv[0] != "/opt/sdk/claude" || !slices.Contains(e.argv, "--output-format") {
		t.Errorf("exec %q in %s", e.argv, e.dir)
	}
	if e.env["GITHUB_TOKEN"] != "" || e.env["CLAUDE_CONFIG_DIR"] != w.be.role.ClaudeDir || !strings.HasPrefix(e.env["HTTPS_PROXY"], "http://rubi:") {
		t.Errorf("env %v", e.env)
	}
	eventually(t, "the role released", func() bool { return w.be.releasedCount() >= 1 })
}

func TestStreamedSessionRefusals(t *testing.T) {
	w, h := streamWorld(t)
	h.readyErr = errors.New("only 300 MiB of memory are free")
	if code, _, errOut := w.runShim("", nil, w.argv()...); code == 0 || !strings.Contains(errOut, "300 MiB") {
		t.Errorf("not ready: %d %q", code, errOut)
	}
	h.readyErr = nil
	// a folder outside the role's project
	outside := t.TempDir()
	w.cwd = outside
	if code, _, errOut := w.runShim("", nil, t3ArgvFor(outside, w.att, "bypassPermissions")...); code == 0 || errOut == "" {
		t.Errorf("outside the project: %d %q", code, errOut)
	}
	if len(h.all()) != 0 {
		t.Error("a VM was started for a refused session")
	}
}

func TestStreamedSessionStopsTheVMWhenClaudeDoesntStart(t *testing.T) {
	w, h := streamWorld(t)
	h.mu.Lock()
	h.execErr = errors.New("no such file")
	h.mu.Unlock()
	code, _, errOut := w.runShim("x", nil, w.argv()...)
	if code == 0 || !strings.Contains(errOut, "didn't start in the VM") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
	vm := h.all()[0]
	eventually(t, "the VM stopped", func() bool { s, _ := vm.snap(); return s })
}

func TestStreamedVersionAsksAThrowawayVM(t *testing.T) {
	w, h := streamWorld(t)
	code, out, _ := w.runShim("", nil, "--version")
	if code != 0 || out != "VERSION 9.9\n" {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	vms := h.all()
	if len(vms) != 1 || len(vms[0].mounts) != 0 {
		t.Fatalf("version VMs %d (mounts %v)", len(vms), vms[0].mounts)
	}
	if s, ex := vms[0].snap(); !s || len(ex) != 1 || !slices.Equal(ex[0].argv, []string{"/opt/sdk/claude", "--version"}) {
		t.Errorf("stopped %v, execs %v", s, ex)
	}
	// remembered: a second ask starts no VM
	if code, out, _ := w.runShim("", nil, "--version"); code != 0 || out != "VERSION 9.9\n" || len(h.all()) != 1 {
		t.Errorf("second ask: %d %q, %d VMs", code, out, len(h.all()))
	}
}

func TestClaudeForArch(t *testing.T) {
	if !strings.HasSuffix(claudeForArch("amd64"), "claude-agent-sdk-linux-x64/claude") || !strings.HasSuffix(claudeForArch("arm64"), "claude-agent-sdk-linux-arm64/claude") {
		t.Error(claudeForArch("amd64"), claudeForArch("arm64"))
	}
}

func TestSessionSpecMountsKeepThePolicyApart(t *testing.T) {
	w, _ := streamWorld(t)
	files, err := w.srv.writeFiles()
	if err != nil {
		t.Fatal(err)
	}
	spec := SessionSpec{Name: "rubi-t3-x", Role: testRole, ClaudeDir: w.be.role.ClaudeDir, ContentDir: w.be.role.ContentDir,
		Cwd: w.cwd, TrustedRoot: w.project, HelperDir: w.dir, SignInDir: files.signIn, HookScript: files.hook,
		ManagedSettings: files.settings, T3Port: testPort}
	pol, err := w.srv.policyDirs(files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Mounts(pol); err != nil {
		t.Fatalf("the real policy folders: %v", err)
	}
	if _, err := spec.Mounts([]agent.Mount{{Source: pol[0].Source, Target: pol[0].Target}}); err == nil {
		t.Error("a read-write policy folder was accepted")
	}
	// a policy folder inside a read-write folder of the session (the role's claude directory) would be the VM's to change
	inside := filepath.Join(w.be.role.ClaudeDir, "policy")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Mounts([]agent.Mount{{Source: inside, Target: "/etc/rubi", ReadOnly: true}}); err == nil {
		t.Error("a policy folder inside a read-write folder was accepted")
	}
	bad := spec
	bad.T3Port = 3129
	if _, err := bad.Mounts(pol); err == nil {
		t.Error("a forwarder port was accepted as T3's")
	}
}
