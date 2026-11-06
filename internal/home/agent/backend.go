package agent

import (
	"context"
	"path/filepath"
)

// The VM backend: what Rubi Home runs agents' VMs with (docs/plans/home-platforms.md, stage P8 and section 2.7). Home asks a
// backend for everything that depends on how VMs are made, and never names a tool: Apple's container on macOS today, a QEMU/KVM
// microvm on Linux later (stage P9), and "none" where there is no backend, which makes agents and T3 sessions "not available on
// this computer" (package home: features.go).
//
// What a backend has to give, and where the interface says so:
//
//   - lifecycle and exec with stdio: New returns a VM (Start, Exec, Stop, State; vm.go). Exec and Start hand back a Conn, a
//     process's stdin and stdout, whatever carries them (a CLI's pipes, a guest agent's channel).
//   - shared folders: Project.Mounts, the one list of host folders a VM shows (the project read-write, the role's content and
//     the shield read-only). A backend turns each into its own kind of mount (a bind mount, a virtio-fs share).
//   - a control channel with no network card: StartSpec.Pipe and package netpipe. The VM has no network at all; the proxy, the
//     hook gate and T3's port are streams over the channel the backend provides.
//   - resource limits: VMLimits.
//   - image: Provisioner.Image and BuildImage (a built image today, a downloaded kernel and root file system later).
//   - setup: Provisioner, the wizard's steps for this backend.
//   - sessions of T3 and the provider sign-in, which also run in VMs: Sessions and Login.

// Availability says whether a backend can run on this computer, and if not, why. Reason is the inside of the sentence
// "That isn't available on this computer (...)." and is empty when OK.
type Availability struct {
	OK     bool
	Reason string
}

// VMLimits are the resources of a VM ("" = the backend's defaults).
type VMLimits struct {
	CPUs   string
	Memory string
}

// BackendConfig is what a backend is made from.
type BackendConfig struct {
	// Bin is the backend tool's path ("" = where the system installs it).
	Bin string
	// RunnerDir is the directory with the runner sources the image is built from ("" = the runner bundled in this binary,
	// written to EnvDir/runner-build/<hash>); EnvDir is Home's folder, where the exec's environment file is written.
	RunnerDir string
	EnvDir    string
	Limits    VMLimits
}

// Mount is a folder of the computer shown inside a VM.
type Mount struct {
	Source   string // the folder on the computer
	Target   string // where the VM sees it
	ReadOnly bool
}

// Mounts are the shared folders of a project's VM, in the order they are applied (a later one sits on top of an earlier): the
// project at /work; for a role VM also the empty read-only folder over /work/.rubi (the legacy role's state), each of the
// project's own read-only overlays (.claude, .git/hooks) over its place in /work, the role's content read-only at
// RoleMountDir and its config directory read-write at RoleClaudeDir.
func (p Project) Mounts() []Mount {
	ms := []Mount{{Source: p.Dir, Target: "/work"}}
	if p.Role == "" {
		return ms
	}
	if p.EmptyDir != "" {
		ms = append(ms, Mount{Source: p.EmptyDir, Target: "/work/.rubi", ReadOnly: true})
	}
	for _, m := range p.ReadOnly {
		ms = append(ms, Mount{Source: m.Source, Target: "/work/" + filepath.ToSlash(m.Name), ReadOnly: true})
	}
	return append(ms,
		Mount{Source: p.RoleDir, Target: RoleMountDir, ReadOnly: true},
		Mount{Source: p.ClaudeDir, Target: RoleClaudeDir})
}

// CmdRunner runs the commands of a backend's tool; the setup wizard gives its own (a fake in tests).
type CmdRunner interface {
	// Run runs argv to completion and returns its combined output.
	Run(ctx context.Context, argv []string) ([]byte, error)
	// Stream runs argv, giving each line of its output to line, and returns when it ends.
	Stream(ctx context.Context, argv []string, line func(string)) error
}

// States of a provisioning step, the words of the setup wizard.
const (
	StepTodo   = "todo"
	StepDone   = "done"
	StepFailed = "failed"
)

// StepStatus is how one provisioning step stands. Action names the button that moves it on ("install", "upgrade", "start",
// "build"); Reason is a sentence for the user.
type StepStatus struct {
	State  string
	Reason string
	Action string
}

// Titles are the names the wizard shows for a backend's steps.
type Titles struct {
	Tool    string // the program VMs run with
	Service string // what must be running for it
}

// Provisioner is the setup wizard's part of a backend: the steps before any project is added. Nothing here changes the
// computer except StartService and BuildImage; installing the tool is the wizard's own step (setup/container.go for Apple's).
type Provisioner interface {
	Titles() Titles
	// Tool says whether the tool is installed and at least version want ("1.6.1", the one Rubi Home installs).
	Tool(ctx context.Context, run CmdRunner, want string) StepStatus
	// Installed reads the installed tool's version ("1.6.1"): present is false when there is none, readable false when it
	// could not be read.
	Installed(ctx context.Context, run CmdRunner) (version string, readable, present bool)
	// Service says whether what the tool needs is running.
	Service(ctx context.Context, run CmdRunner) StepStatus
	// StartService starts it; log gets the tool's output. The wizard asks Service again to see whether it came up.
	StartService(ctx context.Context, run CmdRunner, log func(string)) error
	// ServiceDetail is the raw status of the service, for an error message when it did not come up.
	ServiceDetail(ctx context.Context, run CmdRunner) string
	// Image says whether the runner image is there.
	Image(ctx context.Context, run CmdRunner) StepStatus
	// BuildImage builds it, giving log the tool's output. prepared is false when the build did not start (the sources could
	// not be made ready); err then says why.
	BuildImage(ctx context.Context, run CmdRunner, log func(string)) (prepared bool, err error)
	// ImageTag names the image.
	ImageTag() string
}

// ToolInstaller is a Provisioner whose tool Rubi Home installs by itself, with no administrator: the microvm backend's VM
// files, downloaded from Home's own release and checked against its signed SHA256SUMS. The setup wizard's tool step calls
// InstallTool instead of the Mac's administrator package install; it says how it went through log.
type ToolInstaller interface {
	InstallTool(ctx context.Context, log func(string)) error
}

// SessionHost is the backend's part of the T3 session server (package t3vm): the VMs of T3 sessions are made, inspected and
// removed through it.
type SessionHost interface {
	EnsureImage(ctx context.Context) error
	ImageTag() string
	// ContainerBin is the tool's path, handed to the session spec that builds the VM's command lines.
	ContainerBin() string
	RunDetached(ctx context.Context, argv []string) error
	RunOutput(ctx context.Context, argv []string) ([]byte, error)
	// ListContainers names every VM, and InspectMounts says what a VM has mounted (the sweep at Home's start, and the check
	// after a VM is run).
	ListContainers(ctx context.Context) ([]string, error)
	InspectMounts(ctx context.Context, name string) ([]ContainerMount, error)
	StartSessionPipe(ctx context.Context, name string, spec *PipeSpec, env map[string]string, giveUp func()) (stop func(), err error)
	RemoveContainer(ctx context.Context, name string) error
}

// LoginHost starts the VM the provider sign-in runs in: no project, no network but the allow-list proxy for the sign-in hosts,
// the shared sign-in folder mounted read-write.
type LoginHost interface {
	StartLogin(ctx context.Context, signInDir string) (*AuthSession, error)
}

// VMBackend is one way of running agents' VMs.
type VMBackend interface {
	// Name is a short stable name: "apple-container", later "microvm"; "none" where there is no backend.
	Name() string
	// Available says, without changing anything, whether this computer can run the backend's VMs at all (the right system,
	// hardware virtualisation, and so on). Whether its tools and image are installed is the Provisioner's business.
	Available(ctx context.Context) Availability
	// New is the VM of a project (or of one isolated role of it); it is not started.
	New(p Project) VM
	// Sweep ends what a crash left behind: sign-in VMs.
	Sweep(ctx context.Context)
	// Login, Sessions and Provision are the sign-in, the T3 sessions and the setup wizard's parts. They are nil when
	// Available says no.
	Login() LoginHost
	Sessions() SessionHost
	Provision() Provisioner
}

// NoBackend is the backend of a computer that has none: it is not available, for the given reason, and makes no VM.
func NoBackend(reason string) VMBackend { return noBackend{reason: reason} }

type noBackend struct{ reason string }

func (noBackend) Name() string { return "none" }
func (n noBackend) Available(context.Context) Availability {
	return Availability{Reason: n.reason}
}
func (noBackend) New(Project) VM         { return unavailableVM{} }
func (noBackend) Sweep(context.Context)  {}
func (noBackend) Login() LoginHost       { return nil }
func (noBackend) Sessions() SessionHost  { return nil }
func (noBackend) Provision() Provisioner { return nil }

// unavailableVM is what New gives where there is no backend: it never starts anything.
type unavailableVM struct{}

func (unavailableVM) Start(context.Context, StartSpec) (Conn, error) { return nil, errNoBackend }
func (unavailableVM) Exec(context.Context, []string, map[string]string) (Conn, error) {
	return nil, errNoBackend
}
func (unavailableVM) Stop(context.Context) error { return nil }
func (unavailableVM) State() State               { return StateStopped }

// PlatformBackend is the backend of the system Rubi Home runs on.
func PlatformBackend(cfg BackendConfig) VMBackend { return platformBackend(cfg) }
