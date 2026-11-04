package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var errNoBackend = errors.New("agents are not available on this computer")

// AppleBackend is the VM backend of Apple's container tool (AppleContainer): one VM per container, made by the tool's CLI.
// The tool's name and where it lives come from the system's own file (apple_darwin.go); everything else is plain Go, so its
// command lines are tested on every system.
type AppleBackend struct{ cfg BackendConfig }

// NewAppleBackend makes the Apple container backend. Platform code picks it only on macOS (PlatformBackend).
func NewAppleBackend(cfg BackendConfig) *AppleBackend { return &AppleBackend{cfg: cfg} }

func (b *AppleBackend) container() *AppleContainer {
	return &AppleContainer{Bin: b.cfg.Bin, RunnerDir: b.cfg.RunnerDir, EnvDir: b.cfg.EnvDir, CPUs: b.cfg.Limits.CPUs, Memory: b.cfg.Limits.Memory}
}

// Name implements VMBackend.
func (*AppleBackend) Name() string { return "apple-container" }

// Available implements VMBackend: the backend exists where the platform chose it; the tool being installed is the
// Provisioner's question.
func (*AppleBackend) Available(context.Context) Availability { return Availability{OK: true} }

// New implements VMBackend.
func (b *AppleBackend) New(Project) VM { return b.container() }

// Sweep implements VMBackend: the sign-in containers a crash left (the T3 roles', and the earlier design's).
func (b *AppleBackend) Sweep(ctx context.Context) { b.container().Sweep(ctx) }

// Login implements VMBackend.
func (b *AppleBackend) Login() LoginHost { return b.container() }

// Sessions implements VMBackend.
func (b *AppleBackend) Sessions() SessionHost { return b.container() }

// Provision implements VMBackend.
func (b *AppleBackend) Provision() Provisioner { return appleProvisioner{b: b} }

// appleProvisioner is the setup wizard's view of the Apple container tool: its version, its service (`system`), the runner
// image.
type appleProvisioner struct{ b *AppleBackend }

func (appleProvisioner) Titles() Titles {
	return Titles{Tool: "Apple container", Service: "Container service"}
}

func (p appleProvisioner) bin() string { return p.b.container().bin() }

func (p appleProvisioner) ImageTag() string { return p.b.container().ImageTag() }

var versionRE = regexp.MustCompile(`version (\d+)\.(\d+)\.(\d+)`)

type appleVersion [3]int

func parseVersion(out []byte) (appleVersion, bool) {
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return appleVersion{}, false
	}
	var v appleVersion
	for i := range v {
		n, err := strconv.Atoi(string(m[i+1]))
		if err != nil {
			return appleVersion{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v appleVersion) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// cmp is -1, 0 or 1.
func (v appleVersion) cmp(o appleVersion) int {
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

// CompareVersions is -1, 0 or 1 for versions in the "1.6.1" form; ok is false when either is not one.
func CompareVersions(a, b string) (cmp int, ok bool) {
	va, oka := parseVersion([]byte("version " + a))
	vb, okb := parseVersion([]byte("version " + b))
	return va.cmp(vb), oka && okb
}

func (p appleProvisioner) Installed(ctx context.Context, run CmdRunner) (string, bool, bool) {
	out, err := run.Run(ctx, []string{p.bin(), "--version"})
	if err != nil {
		return "", false, false
	}
	v, ok := parseVersion(out)
	if !ok {
		return "", false, true
	}
	return v.String(), true, true
}

func (p appleProvisioner) Tool(ctx context.Context, run CmdRunner, want string) StepStatus {
	w, _ := parseVersion([]byte("version " + want))
	have, readable, present := p.Installed(ctx, run)
	v, _ := parseVersion([]byte("version " + have))
	switch {
	case !present:
		return StepStatus{State: StepTodo, Reason: "Apple's container tool isn't installed. Rubi Home installs version " + w.String() + ".", Action: "install"}
	case !readable:
		return StepStatus{State: StepFailed, Reason: "Can't read the version of the installed container tool."}
	case v.cmp(w) < 0:
		return StepStatus{State: StepTodo, Reason: "Version " + v.String() + " is installed; Rubi Home needs " + w.String() + ".", Action: "upgrade"}
	}
	return StepStatus{State: StepDone, Reason: "Version " + v.String() + "."}
}

type appleSystemStatus struct {
	Host struct {
		Architecture string `json:"architecture"`
	} `json:"host"`
	Paths struct {
		AppRoot string `json:"appRoot"`
	} `json:"paths"`
	Status string `json:"status"`
}

var archRE = regexp.MustCompile(`^[a-z0-9_]{1,16}$`)

func (p appleProvisioner) systemStatus(ctx context.Context, run CmdRunner) (appleSystemStatus, []byte, bool) {
	out, _ := run.Run(ctx, []string{p.bin(), "system", "status", "--format", "json"})
	var st appleSystemStatus
	if json.Unmarshal(out, &st) != nil {
		return st, out, false
	}
	return st, out, true
}

func (p appleProvisioner) Service(ctx context.Context, run CmdRunner) StepStatus {
	st, _, ok := p.systemStatus(ctx, run)
	if !ok || st.Status != "running" {
		return StepStatus{State: StepTodo, Reason: "The container service isn't running.", Action: "start"}
	}
	if !archRE.MatchString(st.Host.Architecture) || !filepath.IsAbs(st.Paths.AppRoot) {
		return StepStatus{State: StepFailed, Reason: "The container service reports a layout Rubi Home doesn't understand."}
	}
	kernel := filepath.Join(st.Paths.AppRoot, "kernels", "default.kernel-"+st.Host.Architecture)
	if _, err := os.Stat(kernel); err != nil {
		return StepStatus{State: StepTodo, Reason: "The service runs, but the default kernel isn't installed.", Action: "start"}
	}
	return StepStatus{State: StepDone, Reason: "The service is running."}
}

func (p appleProvisioner) StartService(ctx context.Context, run CmdRunner, log func(string)) error {
	return run.Stream(ctx, []string{p.bin(), "system", "start", "--enable-kernel-install"}, log)
}

func (p appleProvisioner) ServiceDetail(ctx context.Context, run CmdRunner) string {
	_, raw, _ := p.systemStatus(ctx, run)
	return string(raw)
}

func (p appleProvisioner) Image(ctx context.Context, run CmdRunner) StepStatus {
	tag := p.ImageTag()
	if _, err := run.Run(ctx, []string{p.bin(), "image", "inspect", tag}); err != nil {
		return StepStatus{State: StepTodo, Reason: "The runner image " + tag + " isn't built yet.", Action: "build"}
	}
	return StepStatus{State: StepDone, Reason: "The runner image " + tag + " is built."}
}

func (p appleProvisioner) BuildImage(ctx context.Context, run CmdRunner, log func(string)) (bool, error) {
	vm := p.b.container()
	argv, err := vm.BuildCommand()
	if err != nil {
		return false, err
	}
	argv[0] = p.bin()
	log("Building " + vm.ImageTag() + " (this takes a few minutes)...")
	return true, run.Stream(ctx, argv, log)
}

// NormalizeVersion is a version in the "1.6.1" form as three numbers ("0.0.0" when it is none).
func NormalizeVersion(v string) string {
	p, _ := parseVersion([]byte("version " + v))
	return p.String()
}
