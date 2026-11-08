package home

// Memory protection on a shared Linux computer (docs/plans/t3-cloud-agents.md, slice 2): project and sign-in keys are
// unlocked only where Yama keeps every process from reading another's memory (ptrace_scope 3), or, on a kernel without
// Yama, after the owner accepted once that root there can read the agents' memory. The acceptance names the computer
// and the kernel, so a new kernel or another computer asks again. Asking for it (a strong approval) and the key
// unlocks that call MemoryProtection come with the project keys (slice 5).

import (
	"errors"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/hostguard"
)

// MemoryRisk is an acceptance of hostguard.RiskText on one computer and kernel.
type MemoryRisk struct {
	MachineID string    `json:"machine_id"`
	Kernel    string    `json:"kernel"`
	At        time.Time `json:"at"`
}

func (h *Helper) probeHost() (hostguard.Host, error) {
	if h.hostProbe != nil {
		return h.hostProbe()
	}
	return hostguard.Probe()
}

// MemoryProtection says whether keys may be unlocked on this computer now, and why not.
func (h *Helper) MemoryProtection() (bool, string) {
	st := h.MemoryProtectionState()
	if !st.OK {
		return false, st.Reason
	}
	return true, ""
}

// MemoryProtectionState is the memory protection as the T3 status shows it.
type MemoryProtectionState struct {
	Applies  bool   `json:"applies"`  // false where it isn't checked (not Linux)
	OK       bool   `json:"ok"`       // keys may be unlocked here
	Accepted bool   `json:"accepted"` // OK only because the owner accepted the risk on this computer and kernel
	Reason   string `json:"reason"`   // what holds, or why keys won't unlock
}

// MemoryProtectionState reads the computer's memory protection and the owner's acceptance.
func (h *Helper) MemoryProtectionState() MemoryProtectionState {
	host, err := h.probeHost()
	if err != nil {
		return MemoryProtectionState{Applies: true, Reason: "Rubi Home couldn't read this computer's memory protection: " + err.Error()}
	}
	if !host.Yama.Applicable {
		return MemoryProtectionState{OK: true}
	}
	if host.Dumpable {
		return MemoryProtectionState{Applies: true, Reason: "Rubi Home couldn't make its own memory unreadable to other processes of this user"}
	}
	r := h.Fresh().MemoryRisk
	accepted := r != nil && r.MachineID != "" && r.MachineID == host.MachineID && r.Kernel == host.Kernel
	ok, why := host.Yama.Decide(accepted)
	st := MemoryProtectionState{Applies: true, OK: ok, Reason: why}
	switch {
	case ok && host.Yama.Present:
		st.Reason = "No process can read another's memory (Yama ptrace_scope 3)."
	case ok:
		st.Accepted, st.Reason = true, "The owner accepted that "+hostguard.RiskText+" (this computer and kernel)."
	}
	return st
}

// AcceptMemoryRisk records the owner's acceptance for this computer and kernel. It is refused where nothing needs
// accepting (Yama present, or not Linux), so an acceptance never stands in for ptrace_scope 3.
func (h *Helper) AcceptMemoryRisk() error {
	host, err := h.probeHost()
	if err != nil {
		return err
	}
	if !host.Yama.Applicable || host.Yama.Present {
		return errors.New("this computer doesn't need that acceptance")
	}
	if host.MachineID == "" || host.Kernel == "" {
		return errors.New("this computer has no machine id or kernel release to bind the acceptance to")
	}
	return h.Update(func(c *Config) {
		c.MemoryRisk = &MemoryRisk{MachineID: host.MachineID, Kernel: host.Kernel, At: time.Now().UTC()}
	})
}
