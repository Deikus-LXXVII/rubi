package home

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// TestLiveSetupStatus probes this computer through a fresh Rubi Home directory and prints the setup page's
// status. Skipped unless RUBI_AGENT_LIVE=1; RUBI_AGENT_LIVE_PROJECT (optional) is added as an agent project.
// Status alone only reads; RUBI_AGENT_LIVE_RUN may run the service or image step (no privilege needed).
func TestLiveSetupStatus(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" {
		t.Skip("set RUBI_AGENT_LIVE=1 to probe this computer")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("RUBI_AGENT_LIVE_PROJECT"); p != "" {
		if err := h.Update(func(c *Config) { c.AgentProjects = []string{p} }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	// RUBI_AGENT_LIVE_RUN names one unprivileged step to run first (service, image).
	if step := os.Getenv("RUBI_AGENT_LIVE_RUN"); step == "service" || step == "image" {
		if err := h.Setup.Run(ctx, step, "", "", ""); err != nil {
			t.Fatal(err)
		}
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(2 * time.Second)
		}
	}
	// RUBI_AGENT_LIVE_RUN=login-url starts the shared sign-in, waits for its link and cancels: no code is sent.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "login-url" {
		if err := h.Setup.Run(ctx, "login", "", "claudeai", ""); err != nil {
			t.Fatal(err)
		}
		url := ""
		for i := 0; i < 90 && url == "" && h.Setup.Busy(); i++ {
			time.Sleep(2 * time.Second)
			if l := h.Setup.Status(ctx).Login; l != nil {
				url = l.URL
			}
		}
		h.Setup.Cancel()
		for h.Setup.Busy() && ctx.Err() == nil {
			time.Sleep(time.Second)
		}
		if !strings.HasPrefix(url, "https://claude.com/") {
			t.Errorf("no sign-in link: %q", url)
		} else {
			t.Logf("sign-in link host ok (%d characters)", len(url))
		}
	}
	// RUBI_AGENT_LIVE_RUN=share: two project VMs run at once, both mount the sign-in directory read-only, and a
	// sign-in-style container with it read-write writes a dummy file (never a token) that both see.
	if os.Getenv("RUBI_AGENT_LIVE_RUN") == "share" {
		liveShare(ctx, t, h)
	}
	b, _ := json.MarshalIndent(h.Setup.Status(ctx), "", "  ")
	t.Logf("status:\n%s", b)
}

func liveShare(ctx context.Context, t *testing.T, h *Helper) {
	t.Helper()
	cfg := h.Fresh()
	a := &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, Network: cfg.AgentNetwork, EnvDir: h.Dir, AuthDir: cfg.AgentAuthDir}
	if err := a.EnsureImage(ctx); err != nil {
		t.Fatalf("image: %v", err)
	}
	if err := a.EnsureAuthDir(); err != nil {
		t.Fatal(err)
	}
	authDir := cfg.AgentAuthDir
	if authDir == "" {
		authDir = agent.AuthDirOf(h.Dir)
	}
	const dummy = "dummy-share-check" // a made-up name and content: this test never touches a real token
	defer os.Remove(filepath.Join(authDir, "token", dummy))

	projects := []agent.Project{{ID: "share-a", Dir: t.TempDir()}, {ID: "share-b", Dir: t.TempDir()}}
	for i := range projects {
		dir, err := filepath.EvalSymlinks(projects[i].Dir)
		if err != nil {
			t.Fatal(err)
		}
		projects[i].Dir = dir
		args := a.RunArgs(projects[i])
		var name string
		for j, x := range args {
			if x == "--name" {
				name = args[j+1]
			}
		}
		_ = exec.Command(args[0], "delete", "--force", name).Run()
		if out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("starting %s: %v: %s", projects[i].ID, err, out)
		}
		defer exec.Command(args[0], "delete", "--force", name).Run()
	}

	// The sign-in-style container: the directory read-write.
	sess, err := a.StartAuth(ctx)
	if err != nil {
		t.Fatalf("sign-in container: %v", err)
	}
	defer sess.Close()
	c, err := sess.Exec(ctx, []string{"sh", "-c", "echo not-a-token > /rubi-auth/token/" + dummy + " && echo wrote"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWrite()
	out, _ := io.ReadAll(c)
	_ = c.Close()
	if !strings.Contains(string(out), "wrote") {
		t.Fatalf("the sign-in container couldn't write: %q", out)
	}

	for _, p := range projects {
		argv := a.ExecArgs(p, "", []string{"sh", "-c",
			"cat /rubi-auth/token/" + dummy + "; touch /rubi-auth/token/other 2>&1; echo touch-rc=$?; ln -s x /rubi-auth/token/link 2>&1; echo ln-rc=$?"})
		got, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", p.ID, err, got)
		}
		res := string(got)
		t.Logf("%s sees: %s", p.ID, strings.ReplaceAll(strings.TrimSpace(res), "\n", " | "))
		if !strings.Contains(res, "not-a-token") {
			t.Errorf("%s doesn't see the file the other container wrote", p.ID)
		}
		if strings.Contains(res, "touch-rc=0") || strings.Contains(res, "ln-rc=0") {
			t.Errorf("%s could write to the read-only directory", p.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(authDir, "token", "other")); err == nil {
		t.Error("a read-only VM created a file")
	}
}
