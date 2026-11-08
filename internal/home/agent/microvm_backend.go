//go:build unix

package agent

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/microvm"
)

// MicroVMBackend runs agents' VMs with QEMU/KVM on Linux (docs/design/linux-microvm.md, stage P9): each VM is a
// MicroVM, its files (kernel, root file system, QEMU, virtiofsd) are the release's own, downloaded and checked by the
// setup wizard, and its file servers run confined by Landlock through Home's own vm-confine command.
type MicroVMBackend struct {
	cfg   BackendConfig
	files microVMFiles
	// exe is Rubi Home's own binary (its vm-confine command confines virtiofsd).
	exe string
	// kvm and landlock read the computer; seams for tests.
	kvm      func() (ok bool, reason string)
	landlock func() error
	// memAvailable is the computer's free memory in MiB (hostguard.MemAvailable when nil); a seam for tests.
	memAvailable func() (int, bool)
	// sessMu guards recentSessions: when session VMs started, in the last sessionWarmup (their memory is not yet in
	// use, so the computer's free memory doesn't show it).
	sessMu         sync.Mutex
	recentSessions []time.Time
	now            func() time.Time
}

// NewMicroVMBackend is the Linux backend of Rubi Home version (its files are that release's) whose binary is exe.
func NewMicroVMBackend(cfg BackendConfig, version, exe string) *MicroVMBackend {
	arch := hostArch()
	return &MicroVMBackend{
		cfg:   cfg,
		files: microVMFiles{Root: filepath.Join(cfg.EnvDir, "vm-files"), Version: version, Arch: arch},
		exe:   exe,
		kvm: func() (bool, string) {
			p := microvm.HostKVMProbe()
			return microvm.CheckKVM(p)
		},
		landlock: func() error { _, err := microvm.LandlockABI(); return err },
	}
}

// hostArch is this computer's CPU in microvm's words ("" when it has no guest).
func hostArch() microvm.Arch {
	switch runtime.GOARCH {
	case "amd64":
		return microvm.X86_64
	case "arm64":
		return microvm.AArch64
	}
	return ""
}

// ReasonNoLandlock is why a Linux without Landlock gets no agents: its file servers would run unconfined.
const ReasonNoLandlock = "agents need a Linux that can confine the VM's file server (Landlock, Linux 5.13 or later, turned on)"

func (b *MicroVMBackend) Name() string { return "microvm" }

// Available: a CPU with a guest, hardware virtualisation and Landlock. A /dev/kvm the user may not open yet is still
// available: an administrator can fix that, and the setup wizard's service step says how.
func (b *MicroVMBackend) Available(context.Context) Availability {
	if b.files.Arch == "" {
		return Availability{Reason: microvm.ReasonNoArch}
	}
	if err := b.landlock(); err != nil {
		return Availability{Reason: ReasonNoLandlock}
	}
	if ok, reason := b.kvm(); !ok && reason != microvm.ReasonKVMDenied {
		return Availability{Reason: reason}
	}
	return Availability{OK: true}
}

// stateDir holds the VMs' folders (sockets, pid files).
func (b *MicroVMBackend) stateDir() string { return filepath.Join(b.cfg.EnvDir, "vm") }

// New is the VM of a project, from this release's files. A VM whose files aren't installed fails its start.
func (b *MicroVMBackend) New(p Project) VM {
	f := b.files
	env, _ := f.LoadImageEnv()
	return &MicroVM{
		Launcher: microvm.Launcher{Virtiofsd: f.Virtiofsd(), Libs: f.Libs(), Confine: []string{b.exe, "vm-confine"},
			IDs: microvm.HostIDs{UID: getuid(), GID: getgid()}},
		QEMU: f.QEMU(), Firmware: f.Firmware(), Kernel: f.Kernel(), Rootfs: f.Rootfs(), Arch: f.Arch,
		ImageEnv: env, StateDir: b.stateDir(), CPUs: b.cfg.Limits.CPUs, Memory: b.cfg.Limits.Memory,
		ready: b.ready,
	}
}

// ready says whether a VM can start now: the files installed and KVM usable.
func (b *MicroVMBackend) ready() error {
	if !b.files.Present() {
		return errors.New("the VM files aren't installed yet: finish the agents' setup")
	}
	if ok, reason := b.kvm(); !ok {
		return errors.New(reason)
	}
	return nil
}

// Sweep ends the VM processes a Home that stopped without ending them left.
func (b *MicroVMBackend) Sweep(context.Context) {
	if err := (microvm.Sweeper{Root: b.stateDir()}).Sweep(); err != nil {
		log.Printf("agent: sweeping the VMs left behind: %v", err)
	}
}

// Login and Sessions: the sign-in VM and T3's session VMs on this backend are later steps of P9.
func (b *MicroVMBackend) Login() LoginHost      { return nil }
func (b *MicroVMBackend) Sessions() SessionHost { return nil }

func (b *MicroVMBackend) Provision() Provisioner { return microVMProvisioner{b: b} }

// microVMProvisioner is the setup wizard's part: the tool step is the VM files, the service step is KVM, and the image
// (the root file system) comes with the files.
type microVMProvisioner struct{ b *MicroVMBackend }

func (p microVMProvisioner) Titles() Titles {
	return Titles{Tool: "VM files (QEMU, the file server, the guest)", Service: "Hardware virtualisation (KVM)"}
}

func (p microVMProvisioner) Tool(context.Context, CmdRunner, string) StepStatus {
	if p.b.files.Present() {
		return StepStatus{State: StepDone, Reason: "The VM files of " + p.b.files.Version + " are installed."}
	}
	return StepStatus{State: StepTodo, Reason: "The VM files aren't installed yet. Rubi Home downloads them from its own release and checks their signature.", Action: "install"}
}

func (p microVMProvisioner) Installed(context.Context, CmdRunner) (string, bool, bool) {
	if p.b.files.Present() {
		return p.b.files.Version, true, true
	}
	return "", true, false
}

func (p microVMProvisioner) Service(context.Context, CmdRunner) StepStatus {
	if ok, reason := p.b.kvm(); !ok {
		return StepStatus{State: StepFailed, Reason: "KVM can't be used yet (" + reason + ")."}
	}
	return StepStatus{State: StepDone, Reason: "KVM is ready."}
}

// StartService: nothing Rubi Home may start; only an administrator can give the user KVM.
func (p microVMProvisioner) StartService(context.Context, CmdRunner, func(string)) error {
	_, reason := p.b.kvm()
	return errors.New("KVM can't be started by Rubi Home (" + reason + ")")
}

func (p microVMProvisioner) ServiceDetail(context.Context, CmdRunner) string {
	_, reason := p.b.kvm()
	return reason
}

func (p microVMProvisioner) Image(context.Context, CmdRunner) StepStatus {
	if p.b.files.Present() {
		return StepStatus{State: StepDone, Reason: "The guest's root file system came with the VM files."}
	}
	return StepStatus{State: StepTodo, Reason: "The guest's root file system comes with the VM files.", Action: "install"}
}

// BuildImage installs the VM files (the root file system is one of them).
func (p microVMProvisioner) BuildImage(ctx context.Context, _ CmdRunner, log func(string)) (bool, error) {
	return true, p.b.InstallFiles(ctx, log)
}

func (p microVMProvisioner) ImageTag() string { return "rubi-vm-" + p.b.files.Version }

// InstallTool is the tool step's install (ToolInstaller): the VM files.
func (p microVMProvisioner) InstallTool(ctx context.Context, log func(string)) error {
	return p.b.InstallFiles(ctx, log)
}

// InstallFiles downloads and checks this release's VM files (the wizard's install of the tool step).
func (b *MicroVMBackend) InstallFiles(ctx context.Context, log func(string)) error {
	return b.files.Install(ctx, log)
}
