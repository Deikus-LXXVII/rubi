package setup

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// fakeProvisioner is a VM backend that is not Apple's: no command line at all, only the states the wizard shows. The wizard
// must work with it exactly as with Apple's (stage P8: "a fake backend drives the same tests").
type fakeProvisioner struct {
	mu                   sync.Mutex
	tool, service, image agent.StepStatus
	started, built       bool
	buildPrepared        bool
	buildErr             error
	cmds                 int
	agent.VMBackend      // New, Sweep... are not used here
}

func (f *fakeProvisioner) Name() string { return "fake" }
func (f *fakeProvisioner) Available(context.Context) agent.Availability {
	return agent.Availability{OK: true}
}
func (f *fakeProvisioner) Provision() agent.Provisioner { return f }
func (f *fakeProvisioner) Titles() agent.Titles {
	return agent.Titles{Tool: "Fake hypervisor", Service: "Fake service"}
}
func (f *fakeProvisioner) Tool(context.Context, agent.CmdRunner, string) agent.StepStatus {
	return f.tool
}
func (f *fakeProvisioner) Installed(context.Context, agent.CmdRunner) (string, bool, bool) {
	return "9.9.9", true, true
}
func (f *fakeProvisioner) Service(context.Context, agent.CmdRunner) agent.StepStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.started {
		return agent.StepStatus{State: agent.StepDone, Reason: "Fake service runs."}
	}
	return f.service
}
func (f *fakeProvisioner) StartService(context.Context, agent.CmdRunner, func(string)) error {
	f.mu.Lock()
	f.started = true
	f.mu.Unlock()
	return nil
}
func (f *fakeProvisioner) ServiceDetail(context.Context, agent.CmdRunner) string { return "detail" }
func (f *fakeProvisioner) Image(context.Context, agent.CmdRunner) agent.StepStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.built {
		return agent.StepStatus{State: agent.StepDone, Reason: "Fake image is there."}
	}
	return f.image
}
func (f *fakeProvisioner) BuildImage(_ context.Context, _ agent.CmdRunner, log func(string)) (bool, error) {
	log("fetching the fake image")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.buildErr == nil && f.buildPrepared {
		f.built = true
	}
	return f.buildPrepared, f.buildErr
}
func (f *fakeProvisioner) ImageTag() string { return "fake-image" }

func fakeEnv(t *testing.T) (*testEnv, *fakeProvisioner) {
	f := &fakeProvisioner{
		tool:          agent.StepStatus{State: agent.StepDone, Reason: "Fake tool 9.9.9."},
		service:       agent.StepStatus{State: agent.StepTodo, Reason: "Fake service is stopped.", Action: "start"},
		image:         agent.StepStatus{State: agent.StepTodo, Reason: "Fake image is missing.", Action: "build"},
		buildPrepared: true,
	}
	e := newTestEnv(t, func(o *Options) { o.Backend = func() agent.VMBackend { return f } })
	return e, f
}

// The wizard shows the backend's titles and statuses, starts its service and builds its image through the Provisioner, and
// runs not one command of its own.
func TestWizardIsDrivenByAnyBackend(t *testing.T) {
	e, f := fakeEnv(t)
	ctx := context.Background()
	steps := e.s.Status(ctx).Steps
	if steps[0].Title != "Fake hypervisor" || steps[0].State != Done || steps[1].Title != "Fake service" || steps[1].State != Todo ||
		steps[1].Action != "start" || steps[3].State != Todo {
		t.Fatalf("%+v", steps)
	}
	if err := e.s.Run(ctx, StepService, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if !f.started || e.step("service").State != Done {
		t.Fatalf("service: started %v, %+v", f.started, e.step("service"))
	}
	if err := e.s.Run(ctx, StepImage, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if !f.built || e.step("image").State != Done || e.step("image").Title != "Runner image" {
		t.Fatalf("image: built %v, %+v", f.built, e.step("image"))
	}
	if len(e.cmd.calls) != 0 {
		t.Errorf("the wizard ran commands of its own: %v", e.cmd.calls)
	}
}

// A build that could not even start fails with the reason; one that ran and failed says the build failed.
func TestWizardReportsABuildThatCouldNotStart(t *testing.T) {
	e, f := fakeEnv(t)
	f.started = true // the steps before the image are done
	f.buildPrepared, f.buildErr = false, errors.New("no directory to build in")
	if err := e.s.Run(context.Background(), StepImage, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if st := e.step("image"); st.State != Failed || !strings.Contains(st.Reason, "no directory to build in") {
		t.Fatalf("%+v", st)
	}
	f.buildPrepared, f.buildErr = true, errors.New("exit 1")
	if err := e.s.Run(context.Background(), StepImage, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if st := e.step("image"); st.State != Failed || !strings.HasPrefix(st.Reason, "The build failed.") {
		t.Fatalf("%+v", st)
	}
}

// Where there is no backend, every step says so with the backend's reason, nothing is run, and a job refuses.
func TestWizardOnAComputerWithNoBackend(t *testing.T) {
	e := newTestEnv(t, func(o *Options) { o.Backend = noBackendLinux })
	ctx := context.Background()
	st := e.step("container")
	if st.State != Failed || !strings.Contains(st.Reason, "isn't available on this computer (agents don't run on Linux yet)") {
		t.Fatalf("%+v", st)
	}
	if st.Title != "Virtual machine tool" {
		t.Errorf("title %q", st.Title)
	}
	for _, step := range []string{StepContainer, StepService, StepImage} {
		_ = e.s.Run(ctx, step, "", "", "")
		e.wait()
		if r := e.step(step); r.State == Done {
			t.Errorf("%s is done on a computer with no backend", step)
		}
	}
	if len(e.cmd.calls) != 0 {
		t.Errorf("a command was run: %v", e.cmd.calls)
	}
}

// installingProvisioner is a backend whose tool Rubi Home installs by itself (agent.ToolInstaller), like the microvm
// backend's VM files.
type installingProvisioner struct {
	*fakeProvisioner
	installed bool
	err       error
	marks     bool // whether a successful install makes the tool step done
	calls     int
	block     chan struct{}
}

func (f *installingProvisioner) Provision() agent.Provisioner { return f }
func (f *installingProvisioner) Tool(context.Context, agent.CmdRunner, string) agent.StepStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed {
		return agent.StepStatus{State: agent.StepDone, Reason: "Fake files are installed."}
	}
	return agent.StepStatus{State: agent.StepTodo, Reason: "Fake files are missing.", Action: "install"}
}
func (f *installingProvisioner) InstallTool(ctx context.Context, log func(string)) error {
	log("fetching the fake files")
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil && f.marks {
		f.installed = true
	}
	return f.err
}

func installingEnv(t *testing.T) (*testEnv, *installingProvisioner) {
	f := &installingProvisioner{fakeProvisioner: &fakeProvisioner{}, marks: true}
	e := newTestEnv(t, func(o *Options) { o.Backend = func() agent.VMBackend { return f } })
	return e, f
}

// The tool step of a backend that installs its own tool calls it: no package download, no administrator dialog, not one
// command of the wizard's own. A tool already there is not installed again.
func TestToolStepInstallsThroughTheBackend(t *testing.T) {
	e, f := installingEnv(t)
	ctx := context.Background()
	if st := e.step("container"); st.State != Todo || st.Action != "install" {
		t.Fatalf("%+v", st)
	}
	if err := e.s.Run(ctx, StepContainer, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if st := e.step("container"); st.State != Done || f.calls != 1 {
		t.Fatalf("calls %d, %+v", f.calls, st)
	}
	if len(e.cmd.calls) != 0 {
		t.Errorf("the wizard ran commands of its own: %v", e.cmd.calls)
	}
	if err := e.s.Run(ctx, StepContainer, "", "", ""); err != nil {
		t.Fatal(err)
	}
	e.wait()
	if f.calls != 1 {
		t.Errorf("installed again: %d calls", f.calls)
	}
}

// A failed install fails the step with its reason; an install that says it worked but left the step undone fails too.
func TestToolStepReportsAFailedInstall(t *testing.T) {
	e, f := installingEnv(t)
	ctx := context.Background()
	f.err = errors.New("the checksum of the kernel archive doesn't match")
	_ = e.s.Run(ctx, StepContainer, "", "", "")
	e.wait()
	if st := e.step("container"); st.State != Failed || !strings.Contains(st.Reason, "checksum of the kernel archive") {
		t.Fatalf("%+v", st)
	}
	f.err, f.marks = nil, false
	_ = e.s.Run(ctx, StepContainer, "", "", "")
	e.wait()
	if st := e.step("container"); st.State != Failed || !strings.Contains(st.Reason, "still isn't done") {
		t.Fatalf("%+v", st)
	}
}

// Cancelling the install stops it, and the step is left to do.
func TestToolStepInstallCanBeCancelled(t *testing.T) {
	e, f := installingEnv(t)
	f.block = make(chan struct{})
	if err := e.s.Run(context.Background(), StepContainer, "", "", ""); err != nil {
		t.Fatal(err)
	}
	for {
		f.mu.Lock()
		n := f.calls
		f.mu.Unlock()
		if n > 0 {
			break
		}
		runtime.Gosched()
	}
	if !e.s.Cancel() {
		t.Fatal("nothing to cancel")
	}
	e.wait()
	if st := e.step("container"); st.State != Todo || f.installed {
		t.Fatalf("installed %v, %+v", f.installed, st)
	}
}
