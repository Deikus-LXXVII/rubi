// Package hostguard reads what a computer offers to protect the agents' memory and how much memory is free, for a
// Rubi Home that runs T3 and the project VMs on a shared Linux computer (docs/plans/t3-cloud-agents.md, slice 2).
// It only reads; it never changes a kernel setting.
package hostguard

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RiskText is what the one-time acceptance shows when the kernel has no Yama.
const RiskText = "root on this computer can read the agents' memory"

// Yama is the kernel's ptrace restriction as /proc shows it.
type Yama struct {
	// Applicable is false where the check doesn't apply (not Linux). Decide then says yes: that means "nothing to
	// check here", never "protected".
	Applicable bool
	Present    bool // /proc/sys/kernel/yama/ptrace_scope exists
	Scope      int  // its value when Present
}

// Decide says whether keys may be unlocked on this computer: Yama at scope 3 (no process may ptrace another), or no
// Yama at all and the owner accepted RiskText once (accepted). Yama below 3 is refused: raising it is the admin's
// step, and accepting a weaker scope is not offered. The reason is "" when the answer is yes.
func (y Yama) Decide(accepted bool) (bool, string) {
	switch {
	case !y.Applicable:
		return true, ""
	case y.Present && y.Scope >= 3:
		return true, ""
	case y.Present:
		return false, fmt.Sprintf("ptrace_scope is %d on this computer; set kernel.yama.ptrace_scope=3 so no process can read another's memory", y.Scope)
	case accepted:
		return true, ""
	}
	return false, "this computer's kernel has no Yama, so " + RiskText + "; accept that once to go on"
}

// ReadYama reads the Yama state under procRoot ("/proc" on a real computer). A missing ptrace_scope reads as "no Yama",
// also where a container runtime hides it: that leads to the acceptance, whose text says root can read memory, never
// to a silent yes.
func ReadYama(procRoot string) (Yama, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "yama", "ptrace_scope"))
	if errors.Is(err, os.ErrNotExist) {
		return Yama{Applicable: true}, nil
	}
	if err != nil {
		return Yama{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return Yama{}, fmt.Errorf("ptrace_scope reads %q", strings.TrimSpace(string(b)))
	}
	return Yama{Applicable: true, Present: true, Scope: n}, nil
}

// ReadMemAvailable reads MemAvailable in MiB from procRoot's meminfo. It is the machine's figure: a cgroup memory
// limit around Rubi Home isn't taken into account.
func ReadMemAvailable(procRoot string) (int, error) {
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || k != "MemAvailable" {
			continue
		}
		fs := strings.Fields(v)
		if len(fs) != 2 || fs[1] != "kB" {
			return 0, fmt.Errorf("MemAvailable reads %q", strings.TrimSpace(v))
		}
		kb, err := strconv.ParseInt(fs[0], 10, 64)
		if err != nil || kb < 0 {
			return 0, fmt.Errorf("MemAvailable reads %q", strings.TrimSpace(v))
		}
		return int(kb / 1024), nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("meminfo has no MemAvailable")
}

// Host is what an acceptance of RiskText is bound to: the computer and its kernel. A new kernel (which may bring
// Yama) or another computer asks again.
type Host struct {
	Yama      Yama
	Dumpable  bool   // this process can still be read by other processes of its user (SetNonDumpable didn't hold)
	Kernel    string // the kernel release (osrelease)
	MachineID string // /etc/machine-id
}

// ReadHost reads the Yama state, the kernel release under procRoot and the machine id under etcRoot.
func ReadHost(procRoot, etcRoot string) (Host, error) {
	y, err := ReadYama(procRoot)
	if err != nil {
		return Host{}, err
	}
	rel, err := os.ReadFile(filepath.Join(procRoot, "sys", "kernel", "osrelease"))
	if err != nil {
		return Host{}, err
	}
	id, err := os.ReadFile(filepath.Join(etcRoot, "machine-id"))
	if err != nil {
		return Host{}, err
	}
	return Host{Yama: y, Kernel: strings.TrimSpace(string(rel)), MachineID: strings.TrimSpace(string(id))}, nil
}
