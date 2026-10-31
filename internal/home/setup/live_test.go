package setup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// TestLiveIsolationProbe runs the self-test's isolation probe (the real script) in a real project VM and feeds its
// output to checkIsolation. No model call is made. Skipped unless RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=netless.
func TestLiveIsolationProbe(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" || os.Getenv("RUBI_AGENT_LIVE_RUN") != "netless" {
		t.Skip("set RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=netless")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	dir := t.TempDir()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "live-probe")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := agent.New(agent.Options{Dir: dir, Projects: func() []string { return []string{proj} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM { return &agent.AppleContainer{EnvDir: dir} }})
	defer mgr.Close()
	sb, err := mgr.Sandbox(ctx, "live-probe")
	if err != nil {
		t.Fatalf("starting the project VM: %v", err)
	}
	c, err := sb.Exec(ctx, []string{"node", "-e", isolationProbe}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWrite()
	out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
	_ = c.Close()
	t.Logf("probe: %s", out)
	if bad := checkIsolation(out); bad != "" {
		t.Errorf("isolation check failed: %s", bad)
	}
}
