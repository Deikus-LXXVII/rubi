package home

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLiveT3Install does the real install of the pinned Node and T3 into a temporary folder (about 300 MB of
// downloads), starts the server from it, pairs and stops it. HOME is a temporary folder too, so nothing is written to
// the user's own. Run with RUBI_T3_INSTALL_LIVE=1 (and RUBI_T3_INSTALL_DIR=<folder> to keep the install).
func TestLiveT3Install(t *testing.T) {
	if os.Getenv("RUBI_T3_INSTALL_LIVE") != "1" {
		t.Skip("set RUBI_T3_INSTALL_LIVE=1 to download and install the pinned T3 (about 300 MB)")
	}
	t.Setenv("HOME", t.TempDir())
	// RUBI_T3_INSTALL_DIR keeps the install where the caller says (cmd/t3-bump runs the contract tests against it).
	dir := os.Getenv("RUBI_T3_INSTALL_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	h, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	th := h.T3
	if th.platform() == "" {
		t.Skip("no T3 build for this platform")
	}
	th.ServeSessions = func(ctx context.Context) error { <-ctx.Done(); return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	t.Cleanup(th.Stop)

	if err := th.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := th.installedNow(); !ok {
		t.Fatal("not installed after the install")
	}
	if err := th.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pin, _ := th.pin()
	st := th.Status()
	if st["running"] != true || st["paired"] != true || st["server_version"] != pin.Version {
		t.Fatalf("status: %v", st)
	}
	if p := h.Fresh().T3Port; p == 0 || p == 3773 {
		t.Errorf("port %d", p)
	}
	if _, err := th.client().Projects(ctx); err != nil {
		t.Errorf("the paired client: %v", err)
	}
	// The launcher found the checked executable in Home's folder and downloaded nothing of its own.
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".t3", "runtime")); err == nil {
		t.Error("the launcher put a runtime in HOME")
	}
	th.Stop()
	if th.srvAlive() {
		t.Error("still running after stop")
	}
}
