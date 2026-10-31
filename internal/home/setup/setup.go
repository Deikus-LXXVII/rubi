// Package setup is the button-only first-time setup of the agent path in Rubi Home (docs/design/
// home-setup-wizard.md): Apple's container tool, its service, the host-only network, the runner image, the
// projects, each project's Claude sign-in and a self-test. Nothing is stored: every status call probes the
// machine, and a job that was running when Rubi Home stopped is simply gone. Processes are started only
// through Commander, so tests never run a real command.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Step ids, in order (the numbers of the design are the positions here plus one).
const (
	StepContainer = "container"
	StepService   = "service"
	StepNetwork   = "network"
	StepImage     = "image"
	StepProjects  = "projects"
	StepLogin     = "login"
	StepSelfTest  = "selftest"
)

var stepOrder = []string{StepContainer, StepService, StepNetwork, StepImage, StepProjects, StepLogin, StepSelfTest}

var stepTitles = map[string]string{
	StepContainer: "Apple container",
	StepService:   "Container service",
	StepNetwork:   "Host-only network",
	StepImage:     "Runner image",
	StepProjects:  "Projects",
	StepLogin:     "Claude sign-in",
	StepSelfTest:  "Self-test",
}

// States of a step.
const (
	Todo      = "todo"
	Working   = "working"
	Done      = "done"
	Failed    = "failed"
	NeedsUser = "needs_user"
)

// MaxProjects is how many project folders can be added.
const MaxProjects = 20

// Settings is the part of Rubi Home's configuration setup reads and changes.
type Settings struct {
	Projects    []string // agent_projects
	Network     string   // agent_network
	ProxyListen string   // agent_proxy_listen
	ProxyVMHost string   // agent_proxy_vm_host
	RunnerDir   string   // agent_runner_dir
}

// Box is a project's running VM, to run commands in.
type Box interface {
	Exec(ctx context.Context, argv []string, extra map[string]string) (agent.Conn, error)
}

// SelfTestRun is the agent manager's self-test (agent.SelfTest).
type SelfTestRun interface {
	Sandbox(ctx context.Context) (Box, error)
	Turn(ctx context.Context, prompt string, d time.Duration) (agent.TurnResult, error)
	End()
}

// Agents is what setup needs from the agent manager.
type Agents interface {
	Projects() []agent.Project
	VMRunning(id string) bool
	Sandbox(ctx context.Context, id string) (Box, error)
	BeginSelfTest(id string) (SelfTestRun, error)
}

// Options configures a Setup.
type Options struct {
	// Dir is Rubi Home's directory: downloads go to Dir/setup, the runner sources to Dir/runner-build.
	Dir string
	// Settings reads the configuration now; Update changes and saves it.
	Settings func() Settings
	Update   func(func(*Settings)) error
	Agents   Agents
	// Cmd runs every command (nil: the real ones).
	Cmd Commander
	// Release is the container release to install (Pinned when zero).
	Release Release
	// HTTP downloads it (nil: a default client); DownloadHosts and RedirectHosts are the hosts (as in a URL's
	// Host) the download may start at and be redirected to (the defaults are github.com and GitHub's two
	// release asset hosts).
	HTTP          *http.Client
	DownloadHosts []string
	RedirectHosts []string
	// SystemTrees are the folders no project may be in (nil: the macOS system folders).
	SystemTrees []string
	// GOOS is the operating system (runtime.GOOS when empty); the Mac-only parts refuse elsewhere.
	GOOS string
	// Now is the clock.
	Now func() time.Time
}

// Setup runs the steps. It is safe for concurrent use; one job runs at a time.
type Setup struct {
	opt Options

	mu        sync.Mutex
	job       *job
	last      map[string]result // a finished job's outcome, per step (or step|project dir)
	tests     map[string]result // self-test outcomes, per project dir (memory only)
	loginSeen map[string]bool   // the last sign-in probe, per project dir
	choosing  bool              // a folder chooser is open
}

type outcome struct {
	State  string
	Reason string
}

type result struct {
	outcome
	Progress []string
}

type job struct {
	step    string
	project agent.Project // for login and selftest
	started time.Time
	cancel  context.CancelFunc
	prog    ring
	login   *loginState
}

// New makes a Setup.
func New(opt Options) *Setup {
	if opt.Cmd == nil {
		opt.Cmd = osCommander{}
	}
	if opt.GOOS == "" {
		opt.GOOS = runtime.GOOS
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Release.Version == "" {
		opt.Release = Pinned
	}
	if opt.Settings == nil {
		opt.Settings = func() Settings { return Settings{} }
	}
	if len(opt.DownloadHosts) == 0 {
		opt.DownloadHosts = []string{"github.com"}
	}
	if len(opt.RedirectHosts) == 0 {
		opt.RedirectHosts = []string{"objects.githubusercontent.com", "release-assets.githubusercontent.com"}
	}
	return &Setup{opt: opt, last: map[string]result{}, tests: map[string]result{}, loginSeen: map[string]bool{}}
}

func (s *Setup) mac() bool { return s.opt.GOOS == "darwin" }

var errNotMac = errors.New("this works only on macOS")

func (s *Setup) network() string {
	if n := s.opt.Settings().Network; n != "" {
		return n
	}
	return agent.DefaultNetwork
}

// ---- jobs ----

var errBusy = errors.New("busy: another setup job is running")

// begin makes the job; it fails when one runs.
func (s *Setup) begin(step string, p agent.Project) (*job, context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil {
		return nil, nil, errBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{step: step, project: p, started: s.opt.Now().UTC(), cancel: cancel}
	s.job = j
	delete(s.last, key(step, p.Dir))
	return j, ctx, nil
}

func key(step, project string) string {
	if step == StepLogin || step == StepSelfTest {
		return step + "|" + project
	}
	return step
}

// record keeps a step's outcome until the next job of that step (a done step or a cancelled job leaves
// nothing: the probe shows what is there).
func (s *Setup) record(ctx context.Context, step, project string, j *job, out outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(step, project)
	if out.State == Done || ctx.Err() != nil {
		delete(s.last, k)
		return
	}
	s.last[k] = result{outcome: out, Progress: j.prog.snapshot()}
}

func (s *Setup) release(j *job) {
	s.mu.Lock()
	if s.job == j {
		s.job = nil
	}
	s.mu.Unlock()
	j.cancel()
}

// setStep moves a running job (the "all" job) on to the next step.
func (s *Setup) setStep(j *job, step string, p agent.Project) {
	s.mu.Lock()
	j.step, j.project = step, p
	j.prog = ring{}
	delete(s.last, key(step, p.Dir))
	s.mu.Unlock()
}

func (j *job) log(s *Setup) func(string) {
	return func(line string) {
		s.mu.Lock()
		j.prog.add(line)
		s.mu.Unlock()
	}
}

// Cancel stops the running job.
func (s *Setup) Cancel() bool {
	s.mu.Lock()
	j := s.job
	s.mu.Unlock()
	if j == nil {
		return false
	}
	j.cancel()
	return true
}

// Run starts a step as a job. project is a project id (login, selftest, all; the first project when
// empty for all); method is "console" or "claudeai" for login (empty: only check); option is
// "auto-address" for the network step.
func (s *Setup) Run(ctx context.Context, step, project, method, option string) error {
	switch step {
	case StepContainer, StepService, StepNetwork, StepImage, StepLogin, StepSelfTest, "all":
	default:
		return fmt.Errorf("unknown step %q", clip(step, 32))
	}
	if method != "" && method != "console" && method != "claudeai" {
		return errors.New(`method must be "console" or "claudeai"`)
	}
	if option != "" && (option != optAutoAddress || step != StepNetwork) {
		return errors.New("unknown option")
	}
	if s.Busy() {
		return errBusy
	}
	var p agent.Project
	if step == StepLogin || step == StepSelfTest || step == "all" && project != "" {
		var ok bool
		if p, ok = s.findProject(project); !ok {
			if project == "" {
				return errors.New("a project is needed")
			}
			return fmt.Errorf("unknown project %q", clip(project, 64))
		}
	}
	if n := s.firstUnmet(ctx, needs(step)); n > 0 && step != "all" {
		return fmt.Errorf("needs step %d first", n)
	}
	if step == StepSelfTest {
		if st := s.loginState(ctx, p); st.State != Done {
			return fmt.Errorf("needs step %d first", 6)
		}
	}
	j, jctx, err := s.begin(step, p)
	if err != nil {
		return err
	}
	go func() {
		defer s.release(j)
		switch step {
		case StepContainer:
			s.record(jctx, step, "", j, s.runContainer(jctx, j))
		case StepService:
			s.record(jctx, step, "", j, s.runService(jctx, j))
		case StepNetwork:
			s.record(jctx, step, "", j, s.runNetwork(jctx, j, option == optAutoAddress))
		case StepImage:
			s.record(jctx, step, "", j, s.runImage(jctx, j))
		case StepLogin:
			s.record(jctx, step, p.Dir, j, s.runLogin(jctx, j, p, method, false))
		case StepSelfTest:
			s.record(jctx, step, p.Dir, j, s.runSelfTest(jctx, j, p))
		default:
			s.runAll(jctx, j, p, method)
		}
	}()
	return nil
}

// Busy says whether a job runs.
func (s *Setup) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job != nil
}

// needs is how many of the first steps must be done before step runs (0 for none).
func needs(step string) int {
	switch step {
	case StepService:
		return 1
	case StepNetwork:
		return 2
	case StepImage:
		return 3
	case StepLogin, StepSelfTest:
		return 5
	}
	return 0
}

func (s *Setup) findProject(id string) (agent.Project, bool) {
	ps := s.opt.Agents.Projects()
	if id == "" && len(ps) > 0 {
		return ps[0], true
	}
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return agent.Project{}, false
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// runAll runs steps 1 to 4, then 6 and 7 for the project (the first when none is named), skipping what is
// done, and stops at the first step that isn't done.
func (s *Setup) runAll(ctx context.Context, j *job, p agent.Project, method string) {
	runStep := func(step string, fn func() outcome) bool {
		for _, st := range s.rawAll(ctx) {
			if st.ID == step && st.State == Done {
				return true
			}
		}
		s.setStep(j, step, agent.Project{})
		out := fn()
		s.record(ctx, step, "", j, out)
		return out.State == Done
	}
	if !runStep(StepContainer, func() outcome { return s.runContainer(ctx, j) }) ||
		!runStep(StepService, func() outcome { return s.runService(ctx, j) }) ||
		!runStep(StepNetwork, func() outcome { return s.runNetwork(ctx, j, false) }) ||
		!runStep(StepImage, func() outcome { return s.runImage(ctx, j) }) {
		return
	}
	if p.Dir == "" {
		ps := s.opt.Agents.Projects()
		if len(ps) == 0 {
			s.mu.Lock()
			s.last[key(StepProjects, "")] = result{outcome: outcome{NeedsUser, "Add a project folder first."}}
			s.mu.Unlock()
			return
		}
		p = ps[0]
	}
	s.setStep(j, StepLogin, p)
	out := s.runLogin(ctx, j, p, method, true)
	s.record(ctx, StepLogin, p.Dir, j, out)
	if out.State != Done {
		return
	}
	s.setStep(j, StepSelfTest, p)
	s.record(ctx, StepSelfTest, p.Dir, j, s.runSelfTest(ctx, j, p))
}
