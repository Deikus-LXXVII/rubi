// Package setup is the button-only first-time setup of the agent path in Rubi Home (docs/design/
// home-setup-wizard.md): Apple's container tool, its service, the network step (no network is needed any more:
// VMs run with none), the runner image, the
// projects, each project's Claude sign-in and a self-test. Nothing is stored: every status call probes the
// machine, and a job that was running when Rubi Home stopped is simply gone. Processes are started only
// through Commander, so tests never run a real command.
package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// StepLogin is the name of the sign-in job. It is no row of this status: the T3 roles' sign-in (t3-setup) reports its own.
	StepLogin = "login"
)

var stepOrder = [...]string{StepContainer, StepService, StepNetwork, StepImage, StepProjects}

// stepTitles are the names of the steps; the first two are the VM backend's (Provisioner.Titles) when it has them.
var stepTitles = map[string]string{
	StepContainer: "Virtual machine tool",
	StepService:   "Virtual machine service",
	StepNetwork:   "VM network (none needed)",
	StepImage:     "Runner image",
	StepProjects:  "Projects",
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
	Network     string   // agent_network (obsolete: VMs have no network; read, never used)
	ProxyListen string   // agent_proxy_listen (obsolete, as above)
	ProxyVMHost string   // agent_proxy_vm_host (obsolete, as above)
	RunnerDir   string   // agent_runner_dir
}

// Box is a project's running VM, to run commands in.
type Box interface {
	Exec(ctx context.Context, argv []string, extra map[string]string) (agent.Conn, error)
}

// AuthSession is the running sign-in container (agent.AuthSession).
type AuthSession interface {
	Exec(ctx context.Context, argv []string, extra map[string]string) (agent.Conn, error)
	Close()
}

// Agents is what setup needs from the agent manager and the container tool.
type Agents interface {
	Projects() []agent.Project
	// SignedIn says whether the shared sign-in folder holds a credential file. It only looks at the file (a stat);
	// nothing here ever opens it.
	SignedIn(ctx context.Context) bool
	// StartAuth starts the container the sign-in runs in.
	StartAuth(ctx context.Context) (AuthSession, error)
}

// Opener opens a link in the user's browser (home.Opener).
type Opener interface {
	Open(ctx context.Context, link string) error
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
	// Opener opens a link in the browser (nil: nothing can be opened).
	Opener Opener
	// Release is the container release to install (Pinned when zero).
	Release Release
	// HTTP downloads it (nil: a default client); DownloadHosts and RedirectHosts are the hosts (as in a URL's
	// Host) the download may start at and be redirected to (the defaults are github.com and GitHub's two
	// release asset hosts).
	HTTP          *http.Client
	DownloadHosts []string
	RedirectHosts []string
	// SystemTrees are the folders no project may be in (nil: hostpaths.SystemTrees). A tree that holds the user's home
	// folder or a mounted disk the browser starts in does not apply to what is inside that place (a home in
	// /var/home, a disk in /run/media).
	SystemTrees []string
	// BrowseRoots are the mounted disks the folder browser may start in besides the home folder (nil:
	// hostpaths.BrowseRoots).
	BrowseRoots func() []string
	// Backend makes the VM backend the steps probe and build with (nil: the system's, agent.PlatformBackend, made from the
	// runner directory of Settings and Dir). It is called at each use, so a changed setting applies.
	Backend func() agent.VMBackend
	// Now is the clock.
	Now func() time.Time
	// LoginHelper is the sign-in helper run in the sign-in container and LoginEnv the environment it gets besides the
	// proxy's. Without them no sign-in runs (the T3 roles' sign-in sets both).
	LoginHelper string
	LoginEnv    func() map[string]string
}

// Setup runs the steps. It is safe for concurrent use; one job runs at a time.
type Setup struct {
	opt Options

	mu   sync.Mutex
	job  *job
	last map[string]result // a finished job's outcome, per step (or step|project dir)

	// beforeOpen is a test seam: it runs after a folder was checked and before it is opened.
	beforeOpen func(path string)
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
	project agent.Project // for "all" with a project
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
	return &Setup{opt: opt, last: map[string]result{}}
}

// backend is the VM backend now.
func (s *Setup) backend() agent.VMBackend {
	if s.opt.Backend != nil {
		return s.opt.Backend()
	}
	return agent.PlatformBackend(agent.BackendConfig{RunnerDir: s.opt.Settings().RunnerDir, EnvDir: s.opt.Dir})
}

// agentsHere says whether this computer has a VM backend, and if not, why (the inside of "That isn't available on this
// computer (...)").
func (s *Setup) agentsHere(ctx context.Context) agent.Availability {
	return s.backend().Available(ctx)
}

var errNotMac = errors.New("this works only on macOS")

// errNoBackend answers a step that needs a VM backend on a computer that has none.
func errNoBackend(a agent.Availability) error {
	return fmt.Errorf("That isn't available on this computer (%s).", a.Reason)
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

func key(step, project string) string { return step }

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

// Run starts a step as a job. project is a project id ("all" with one names it); method is not used by any step here
// (the sign-in and the self-test of the T3 roles are t3-setup's); option is "auto-address" for the network step
// (accepted and ignored: there is no address any more).
func (s *Setup) Run(ctx context.Context, step, project, method, option string) error {
	switch step {
	case StepContainer, StepService, StepNetwork, StepImage, "all":
	case StepLogin, "signout", "selftest":
		return errors.New("the sign-in and the self-test are part of the T3 Agents setup (t3-setup.run), not of this one")
	default:
		return fmt.Errorf("unknown step %q", clip(step, 32))
	}
	if method != "" {
		return errors.New("this step takes no method")
	}
	if option != "" && (option != optAutoAddress || step != StepNetwork) {
		return errors.New("unknown option")
	}
	if s.Busy() {
		return errBusy
	}
	var p agent.Project
	if step == "all" && project != "" {
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
			s.record(jctx, step, "", j, s.runNetwork(jctx, j))
		case StepImage:
			s.record(jctx, step, "", j, s.runImage(jctx, j))
		default:
			s.runAll(jctx, j, p)
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

// privileged are the steps "all" never runs: installing the tool has its own approval in the plugin, so "all" stops
// when it isn't done.
var privileged = map[string]bool{StepContainer: true}

// runAll runs the service and image steps, skipping what is done. It stops at the first step that isn't done. It never
// installs the container tool: if it is missing, also if it goes missing while "all" runs, it stops there ("needs step
// N (asks for approval)").
func (s *Setup) runAll(ctx context.Context, j *job, p agent.Project) {
	// gate checks that the steps up to and including n are done, now. A privileged one that isn't stops "all".
	gate := func(n int) bool {
		raw := s.rawAll(ctx)
		for i := 0; i < n; i++ {
			if raw[i].State == Done {
				continue
			}
			step := stepOrder[i]
			reason := fmt.Sprintf("Stopped: needs step %d first.", i+1)
			if privileged[step] {
				reason = fmt.Sprintf("Stopped: needs step %d (asks for approval).", i+1)
			}
			s.setStep(j, step, agent.Project{})
			s.mu.Lock()
			s.last[key(step, "")] = result{outcome: outcome{NeedsUser, reason}}
			s.mu.Unlock()
			return false
		}
		return true
	}
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
	if !gate(1) || !runStep(StepService, func() outcome { return s.runService(ctx, j) }) || !gate(3) ||
		!runStep(StepImage, func() outcome { return s.runImage(ctx, j) }) || !gate(4) {
		return
	}
}
