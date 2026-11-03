package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Step is one row of the status. The sign-in (login) is one row for the whole computer: it has no project rows.
type Step struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Reason   string   `json:"reason"`
	Progress []string `json:"progress,omitempty"`
	// Action is the one button the row offers: install, upgrade, start, create, use_auto_address, build,
	// add, check, cancel (while it works).
	Action string `json:"action,omitempty"`
	// Action2 is the second button, when there is one.
	Action2 string `json:"action2,omitempty"`
	// Projects are the rows of the project step.
	Projects []Row `json:"projects,omitempty"`
}

// Row is a project's row. Action is remove (projects).
type Row struct {
	ID       string   `json:"id"`
	Dir      string   `json:"dir"`
	State    string   `json:"state"`
	Reason   string   `json:"reason,omitempty"`
	Progress []string `json:"progress,omitempty"`
	Action   string   `json:"action,omitempty"`
}

// JobInfo is the running job.
type JobInfo struct {
	Step    string    `json:"step"`
	Project string    `json:"project,omitempty"`
	Started time.Time `json:"started"`
}

// LoginInfo is the sign-in that runs: URL is there while it waits for the browser.
type LoginInfo struct {
	URL       string `json:"url,omitempty"`
	WantsCode bool   `json:"wants_code"`
}

// Status is the result of agent-setup.status.
type Status struct {
	Steps []Step     `json:"steps"`
	Job   *JobInfo   `json:"job,omitempty"`
	Login *LoginInfo `json:"login,omitempty"`
}

const optAutoAddress = "auto-address"

// Status probes the machine and reports every step.
func (s *Setup) Status(ctx context.Context) Status {
	raw := s.rawAll(ctx)
	s.mu.Lock()
	var jb *job
	var st Status
	if s.job != nil {
		cp := *s.job
		cp.prog = ring{lines: s.job.prog.snapshot()}
		jb = &cp
		st.Job = &JobInfo{Step: jb.step, Project: jb.project.ID, Started: jb.started}
		if l := s.job.login; l != nil {
			st.Login = &LoginInfo{URL: l.url, WantsCode: l.wantsCode}
		}
	}
	last := map[string]result{}
	for k, v := range s.last {
		last[k] = v
	}
	s.mu.Unlock()

	for _, r := range raw {
		s.overlay(&r, jb, last)
		st.Steps = append(st.Steps, r)
	}
	return st
}

func firstNotDone(steps []Step) int {
	for i, s := range steps {
		if s.State != Done {
			return i
		}
	}
	return len(steps)
}

func aggregate(rows []Row) (string, string) {
	if len(rows) == 0 {
		return Todo, ""
	}
	for _, want := range []string{Working, Failed, NeedsUser, Todo} {
		for _, r := range rows {
			if r.State == want {
				return want, r.Reason
			}
		}
	}
	return Done, fmt.Sprintf("%d of %d projects", len(rows), len(rows))
}

// overlay shows a running job (working, with its progress) or the outcome of the last one (failed or
// needs_user) on a step that isn't done.
func (s *Setup) overlay(r *Step, jb *job, last map[string]result) {
	if r.State == Done {
		return
	}
	if jb != nil && jb.step == r.ID {
		r.State, r.Reason, r.Action, r.Progress = Working, "Working...", "cancel", jb.prog.snapshot()
		return
	}
	if strings.HasPrefix(r.Reason, "Needs step") {
		return
	}
	if l, ok := last[key(r.ID, "")]; ok {
		r.State, r.Reason, r.Progress = l.State, l.Reason, l.Progress
	}
}

// rawAll probes steps 1 to 5 (a step whose earlier step isn't done isn't probed).
func (s *Setup) rawAll(ctx context.Context) []Step {
	probes := []func(context.Context) Step{s.probeContainer, s.probeService, s.probeNetwork, s.probeImage}
	out := make([]Step, 0, 5)
	bad := -1
	for i, p := range probes {
		var st Step
		if bad >= 0 {
			st = Step{State: Todo, Reason: fmt.Sprintf("Needs step %d first", bad+1)}
		} else {
			st = p(ctx)
			if st.State != Done {
				bad = i
			}
		}
		st.ID, st.Title = stepOrder[i], stepTitles[stepOrder[i]]
		out = append(out, st)
	}
	pr := s.probeProjects()
	pr.ID, pr.Title = StepProjects, stepTitles[StepProjects]
	return append(out, pr)
}

// firstUnmet returns the number (1-based) of the first of the first n steps that isn't done, or 0.
func (s *Setup) firstUnmet(ctx context.Context, n int) int {
	if n == 0 {
		return 0
	}
	for i, st := range s.rawAll(ctx)[:n] {
		if st.State != Done {
			return i + 1
		}
	}
	return 0
}

// ---- versions ----

var versionRE = regexp.MustCompile(`version (\d+)\.(\d+)\.(\d+)`)

type version [3]int

func parseVersion(out []byte) (version, bool) {
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range v {
		n, err := strconv.Atoi(string(m[i+1]))
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// cmp is -1, 0 or 1.
func (v version) cmp(o version) int {
	for i := range v {
		switch {
		case v[i] < o[i]:
			return -1
		case v[i] > o[i]:
			return 1
		}
	}
	return 0
}

func (s *Setup) installed(ctx context.Context) (version, bool, bool) {
	out, err := s.opt.Cmd.Run(ctx, []string{"container", "--version"})
	if err != nil {
		return version{}, false, false
	}
	v, ok := parseVersion(out)
	return v, ok, true
}

// ---- probes ----

func (s *Setup) probeContainer(ctx context.Context) Step {
	if !s.mac() {
		return Step{State: Failed, Reason: "Setting up agents works only on a Mac."}
	}
	want, _ := parseVersion([]byte("version " + s.opt.Release.Version))
	v, ok, present := s.installed(ctx)
	switch {
	case !present:
		return Step{State: Todo, Reason: "Apple's container tool isn't installed. Rubi Home installs version " + want.String() + ".", Action: "install"}
	case !ok:
		return Step{State: Failed, Reason: "Can't read the version of the installed container tool."}
	case v.cmp(want) < 0:
		return Step{State: Todo, Reason: "Version " + v.String() + " is installed; Rubi Home needs " + want.String() + ".", Action: "upgrade"}
	}
	return Step{State: Done, Reason: "Version " + v.String() + "."}
}

type systemStatus struct {
	Host struct {
		Architecture string `json:"architecture"`
	} `json:"host"`
	Paths struct {
		AppRoot string `json:"appRoot"`
	} `json:"paths"`
	Status string `json:"status"`
}

var archRE = regexp.MustCompile(`^[a-z0-9_]{1,16}$`)

func (s *Setup) systemStatus(ctx context.Context) (systemStatus, []byte, bool) {
	out, _ := s.opt.Cmd.Run(ctx, []string{"container", "system", "status", "--format", "json"})
	var st systemStatus
	if json.Unmarshal(out, &st) != nil {
		return st, out, false
	}
	return st, out, true
}

func (s *Setup) probeService(ctx context.Context) Step {
	st, _, ok := s.systemStatus(ctx)
	if !ok || st.Status != "running" {
		return Step{State: Todo, Reason: "The container service isn't running.", Action: "start"}
	}
	if !archRE.MatchString(st.Host.Architecture) || !filepath.IsAbs(st.Paths.AppRoot) {
		return Step{State: Failed, Reason: "The container service reports a layout Rubi Home doesn't understand."}
	}
	kernel := filepath.Join(st.Paths.AppRoot, "kernels", "default.kernel-"+st.Host.Architecture)
	if _, err := os.Stat(kernel); err != nil {
		return Step{State: Todo, Reason: "The service runs, but the default kernel isn't installed.", Action: "start"}
	}
	return Step{State: Done, Reason: "The service is running."}
}

// networkNotNeeded is the network step's reason. VMs run with `--network none` and reach the proxy and the Claude
// gateway through Rubi Home (docs/design/agent-runner.md), so there is nothing to create or to check. The step keeps its
// id so that the plugin's list of steps stays the same.
const networkNotNeeded = "Not needed: VMs run with no network at all. They reach the allow-list proxy through Rubi Home."

func (s *Setup) probeNetwork(ctx context.Context) Step {
	return Step{State: Done, Reason: networkNotNeeded}
}

// appleVM is the VM definition the image and the network are checked and built with.
func (s *Setup) appleVM() *agent.AppleContainer {
	cfg := s.opt.Settings()
	return &agent.AppleContainer{Bin: "container", RunnerDir: cfg.RunnerDir, EnvDir: s.opt.Dir}
}

func (s *Setup) probeImage(ctx context.Context) Step {
	tag := s.appleVM().ImageTag()
	if _, err := s.opt.Cmd.Run(ctx, []string{"container", "image", "inspect", tag}); err != nil {
		return Step{State: Todo, Reason: "The runner image " + tag + " isn't built yet.", Action: "build"}
	}
	return Step{State: Done, Reason: "The runner image " + tag + " is built."}
}

// noFolderYet: the agents' folders are added by a Bot the user asks (with the user's approval), not by the setup.
const noFolderYet = "No project folder yet. Ask your Grok Bot to add the folder you want the agents to work in; you approve it."

func (s *Setup) probeProjects() Step {
	ps := s.opt.Agents.Projects()
	st := Step{Action: "add"}
	for _, p := range ps {
		st.Projects = append(st.Projects, Row{ID: p.ID, Dir: p.Dir, State: Done, Action: "remove"})
	}
	switch {
	case len(ps) > 0:
		st.State, st.Reason = Done, fmt.Sprintf("%d project folder(s).", len(ps))
	case len(s.opt.Settings().Projects) > 0:
		st.State, st.Reason = Todo, "None of the configured folders can be used. Add another folder."
	default:
		st.State, st.Reason = Todo, noFolderYet
	}
	if len(ps) >= MaxProjects {
		st.Action = ""
	}
	return st
}
