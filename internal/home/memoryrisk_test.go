package home

import (
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/hostguard"
)

func memHelper(t *testing.T, host *hostguard.Host) *Helper {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.hostProbe = func() (hostguard.Host, error) { return *host, nil }
	return h
}

func TestMemoryProtectionWithoutYamaNeedsTheAcceptance(t *testing.T) {
	host := hostguard.Host{Yama: hostguard.Yama{Applicable: true}, Kernel: "6.12.113+", MachineID: "m1"}
	h := memHelper(t, &host)
	if ok, why := h.MemoryProtection(); ok || !strings.Contains(why, hostguard.RiskText) {
		t.Fatalf("before the acceptance: %v %q", ok, why)
	}
	if err := h.AcceptMemoryRisk(); err != nil {
		t.Fatal(err)
	}
	if ok, why := h.MemoryProtection(); !ok {
		t.Fatalf("after the acceptance: %q", why)
	}
	// It survives a restart of Home (it is in the configuration).
	h2, err := Open(h.Dir)
	if err != nil {
		t.Fatal(err)
	}
	h2.hostProbe = h.hostProbe
	if ok, _ := h2.MemoryProtection(); !ok {
		t.Error("the acceptance didn't survive a restart")
	}
	// A new kernel or another computer asks again.
	host.Kernel = "6.13.0"
	if ok, _ := h.MemoryProtection(); ok {
		t.Error("the acceptance held for another kernel")
	}
	host.Kernel, host.MachineID = "6.12.113+", "m2"
	if ok, _ := h.MemoryProtection(); ok {
		t.Error("the acceptance held on another computer")
	}
}

func TestMemoryProtectionWithYamaNeedsScope3(t *testing.T) {
	host := hostguard.Host{Yama: hostguard.Yama{Applicable: true, Present: true, Scope: 1}, Kernel: "6.1", MachineID: "m1"}
	h := memHelper(t, &host)
	if ok, why := h.MemoryProtection(); ok || !strings.Contains(why, "ptrace_scope is 1") {
		t.Fatalf("scope 1: %v %q", ok, why)
	}
	// No acceptance can stand in for scope 3.
	if err := h.AcceptMemoryRisk(); err == nil {
		t.Fatal("an acceptance was recorded on a computer with Yama")
	}
	if h.Fresh().MemoryRisk != nil {
		t.Fatal("the refused acceptance was kept")
	}
	host.Yama.Scope = 3
	if ok, why := h.MemoryProtection(); !ok {
		t.Fatalf("scope 3: %q", why)
	}
}

func TestMemoryRiskNeedsAComputerToBindTo(t *testing.T) {
	host := hostguard.Host{Yama: hostguard.Yama{Applicable: true}, Kernel: "6.12"}
	h := memHelper(t, &host)
	if err := h.AcceptMemoryRisk(); err == nil {
		t.Fatal("an acceptance without a machine id was recorded")
	}
	// A stored acceptance with an empty machine id never matches.
	if err := h.Update(func(c *Config) { c.MemoryRisk = &MemoryRisk{Kernel: "6.12"} }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.MemoryProtection(); ok {
		t.Error("an acceptance with no machine id held")
	}
}

func TestMemoryProtectionNeedsRubiHomeNonDumpable(t *testing.T) {
	host := hostguard.Host{Yama: hostguard.Yama{Applicable: true, Present: true, Scope: 3}, Kernel: "6.1", MachineID: "m1", Dumpable: true}
	h := memHelper(t, &host)
	if ok, why := h.MemoryProtection(); ok || !strings.Contains(why, "unreadable") {
		t.Fatalf("a dumpable Rubi Home: %v %q", ok, why)
	}
	host.Dumpable = false
	if ok, why := h.MemoryProtection(); !ok {
		t.Fatalf("non-dumpable at scope 3: %q", why)
	}
}
