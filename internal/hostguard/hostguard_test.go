package hostguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeProc(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestYamaDecides(t *testing.T) {
	for _, c := range []struct {
		name     string
		files    map[string]string
		accepted bool
		ok       bool
		why      string
	}{
		{"no yama, not accepted", nil, false, false, RiskText},
		{"no yama, accepted", nil, true, true, ""},
		{"scope 0", map[string]string{"sys/kernel/yama/ptrace_scope": "0\n"}, true, false, "ptrace_scope is 0"},
		{"scope 1", map[string]string{"sys/kernel/yama/ptrace_scope": "1\n"}, true, false, "ptrace_scope is 1"},
		{"scope 2", map[string]string{"sys/kernel/yama/ptrace_scope": "2\n"}, false, false, "ptrace_scope is 2"},
		{"scope 3", map[string]string{"sys/kernel/yama/ptrace_scope": "3\n"}, false, true, ""},
	} {
		y, err := ReadYama(fakeProc(t, c.files))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ok, why := y.Decide(c.accepted)
		if ok != c.ok || (c.why == "" && why != "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: Decide = %v, %q; want %v, %q", c.name, ok, why, c.ok, c.why)
		}
	}
	if ok, _ := (Yama{}).Decide(false); !ok {
		t.Error("where the check doesn't apply it must not block")
	}
}

func TestYamaUnreadableIsAnError(t *testing.T) {
	if _, err := ReadYama(fakeProc(t, map[string]string{"sys/kernel/yama/ptrace_scope": "x"})); err == nil {
		t.Error("a garbled ptrace_scope was accepted")
	}
}

func TestMemAvailable(t *testing.T) {
	root := fakeProc(t, map[string]string{"meminfo": "MemTotal:       16386048 kB\nMemFree:          912345 kB\nMemAvailable:    2975744 kB\n"})
	n, err := ReadMemAvailable(root)
	if err != nil || n != 2906 {
		t.Fatalf("MemAvailable = %d, %v; want 2906", n, err)
	}
	for _, body := range []string{"MemTotal: 1 kB\n", "MemAvailable: lots kB\n", "MemAvailable: 12 MB\n"} {
		if _, err := ReadMemAvailable(fakeProc(t, map[string]string{"meminfo": body})); err == nil {
			t.Errorf("meminfo %q was accepted", body)
		}
	}
}

func TestReadHost(t *testing.T) {
	proc := fakeProc(t, map[string]string{"sys/kernel/osrelease": "6.12.113+\n"})
	etc := fakeProc(t, map[string]string{"machine-id": "abc123\n"})
	h, err := ReadHost(proc, etc)
	if err != nil {
		t.Fatal(err)
	}
	if h.Kernel != "6.12.113+" || h.MachineID != "abc123" || !h.Yama.Applicable || h.Yama.Present {
		t.Fatalf("host %+v", h)
	}
	if _, err := ReadHost(proc, t.TempDir()); err == nil {
		t.Error("a host without a machine id was read")
	}
}
