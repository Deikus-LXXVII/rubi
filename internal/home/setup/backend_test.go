package setup

import (
	"context"
	"errors"
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
