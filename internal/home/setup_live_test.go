package home

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/sshtarget/sshtest"
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
	b, _ := json.MarshalIndent(h.Setup.Status(ctx), "", "  ")
	t.Logf("status:\n%s", b)
}

// TestLiveRoles starts two role VMs on one project (two different stored roles with a tiny skill each) and
// checks inside each VM that /rubi-role/current/role.json is its own, that nothing of the other role is mounted,
// that /rubi-role can't be written and /rubi-claude can. It needs the runner image (built on first use) and the
// container tool; no model call is made. Skipped unless RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=roles.
func TestLiveRoles(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" || os.Getenv("RUBI_AGENT_LIVE_RUN") != "roles" {
		t.Skip("set RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=roles")
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cfg := h.Fresh()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "live-roles")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := agent.New(agent.Options{
		Dir: h.Dir, Projects: func() []string { return []string{proj} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, Network: cfg.AgentNetwork, EnvDir: h.Dir}
		},
	})
	defer mgr.Close()
	put := func(name string) string {
		skill := "---\nname: " + name + "-skill\ndescription: tiny skill of " + name + "\n---\nSay hi.\n"
		sum := sha256.Sum256([]byte(skill))
		sha := hex.EncodeToString(sum[:])
		args, _ := json.Marshal(map[string]any{"base_version": 0, "by": "live-test", "blobs": map[string]string{sha: skill},
			"spec": map[string]any{"project": "live-roles", "name": name, "description": "live " + name, "tools": []string{"Read"},
				"skills": []map[string]any{{"name": name + "-skill", "source": "adhoc", "sha256": sha}}}})
		res, err := mgr.Handle(ctx, "claude-agent.role.put", args)
		if err != nil {
			t.Fatalf("role.put %s: %v", name, err)
		}
		b, _ := json.Marshal(res)
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(b, &out)
		return out.ID
	}
	ids := [2]string{put("live-a"), put("live-b")}
	sbs := [2]*agent.Sandbox{}
	for i, id := range ids {
		if sbs[i], err = mgr.SandboxRole(ctx, "live-roles", id); err != nil {
			t.Fatalf("starting the VM of role %s: %v", id, err)
		}
	}
	for i, sb := range sbs {
		me, other := ids[i], ids[1-i]
		run := func(script string) string {
			t.Helper()
			c, err := sb.Exec(ctx, []string{"sh", "-c", script}, nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = c.CloseWrite()
			out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
			_ = c.Close()
			return string(out)
		}
		out := run("cat /rubi-role/current/role.json; echo; " +
			"(touch /rubi-role/x 2>/dev/null && echo ROLE-WRITABLE) || echo role-ro; " +
			"(echo x > /rubi-role/current/role.json 2>/dev/null && echo ROLE-JSON-WRITABLE) || echo json-ro; " +
			"(touch /rubi-claude/probe 2>/dev/null && echo claude-rw) || echo CLAUDE-NOT-WRITABLE; " +
			"grep -rl " + other + " /rubi-role /rubi-claude 2>/dev/null | head -3; echo grep-done; " +
			"echo env=$RUBI_ROLE_DIR,$RUBI_CLAUDE_DIR; ls /rubi-auth 2>&1 | head -1")
		t.Logf("role %s:\n%s", me, out)
		if !strings.Contains(out, `"role":"`+me+`"`) && !strings.Contains(out, `"role": "`+me+`"`) {
			t.Errorf("role.json isn't role %s's", me)
		}
		for _, bad := range []string{"ROLE-WRITABLE", "ROLE-JSON-WRITABLE", "CLAUDE-NOT-WRITABLE"} {
			if strings.Contains(out, bad) {
				t.Errorf("role %s: %s", me, bad)
			}
		}
		for _, want := range []string{"role-ro", "json-ro", "claude-rw", "env=/rubi-role,/rubi-claude"} {
			if !strings.Contains(out, want) {
				t.Errorf("role %s: missing %q", me, want)
			}
		}
		if before, _, _ := strings.Cut(out, "grep-done"); strings.Contains(before, other) {
			t.Errorf("role %s sees role %s", me, other)
		}
		if strings.Contains(out, "token") || strings.Contains(out, "oauth") {
			t.Errorf("a sign-in is mounted in role %s's VM", me)
		}
	}
}

// TestLiveSSH runs `ssh <target> <command>` inside a role's VM end to end: the wrapper, the runner's socket, Rubi
// Home's ssh.open and an in-process SSH server on this computer's loopback (Home dials it; the VM never does). A
// role with a grant runs "echo hi" and gets "hi" with status 0; a registered target the role isn't granted, an
// unknown one and the forbidden forms are refused. It needs the runner image (built on first use) and the container
// tool; no model call is made. Skipped unless RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=ssh.
func TestLiveSSH(t *testing.T) {
	if os.Getenv("RUBI_AGENT_LIVE") != "1" || os.Getenv("RUBI_AGENT_LIVE_RUN") != "ssh" {
		t.Skip("set RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=ssh")
	}
	srv, err := sshtest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.Handler = func(cmd string, stdin io.Reader, stdout, stderr io.Writer) int {
		switch {
		case cmd == "echo hi":
			_, _ = io.WriteString(stdout, "hi\n")
			return 0
		case cmd == "cat":
			_, _ = io.Copy(stdout, stdin)
			return 0
		case cmd == "exit 7":
			_, _ = io.WriteString(stderr, "bye\n")
			return 7
		}
		_, _ = io.WriteString(stderr, "unexpected command\n")
		return 1
	}
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	cfg := h.Fresh()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(base, "live-ssh")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := agent.New(agent.Options{
		Dir: h.Dir, Projects: func() []string { return []string{proj} }, Wait: time.Minute,
		NewVM: func(agent.Project) agent.VM {
			return &agent.AppleContainer{RunnerDir: cfg.AgentRunnerDir, Network: cfg.AgentNetwork, EnvDir: h.Dir}
		},
	})
	defer mgr.Close()
	call := func(op string, args any) any {
		t.Helper()
		b, _ := json.Marshal(args)
		res, err := mgr.Handle(ctx, "claude-agent."+op, b)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return res
	}
	for _, name := range []string{"test-target", "other-target"} {
		res := call("ssh.register", map[string]any{"name": name, "host": "127.0.0.1", "port": srv.Port, "user": "deploy", "fingerprint": srv.Fingerprint()})
		b, _ := json.Marshal(res)
		var out struct {
			Line string `json:"public_key_line"`
		}
		_ = json.Unmarshal(b, &out)
		if err := srv.Authorize(out.Line); err != nil {
			t.Fatal(err)
		}
	}
	skill := "---\nname: ssh-skill\ndescription: tiny\n---\nSay hi.\n"
	sum := sha256.Sum256([]byte(skill))
	sha := hex.EncodeToString(sum[:])
	res := call("role.put", map[string]any{"base_version": 0, "by": "live-test", "blobs": map[string]string{sha: skill},
		"spec": map[string]any{"project": "live-ssh", "name": "live-ssh", "description": "live ssh", "tools": []string{"Read"},
			"ssh": []string{"test-target"}, "skills": []map[string]any{{"name": "ssh-skill", "source": "adhoc", "sha256": sha}}}})
	b, _ := json.Marshal(res)
	var role struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(b, &role)
	sb, err := mgr.SandboxRole(ctx, "live-ssh", role.ID)
	if err != nil {
		t.Fatalf("starting the VM: %v", err)
	}
	run := func(script, stdin string) string {
		t.Helper()
		c, err := sb.Exec(ctx, []string{"sh", "-c", script + "; echo rc=$?"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if stdin != "" {
			_, _ = io.WriteString(c, stdin)
		}
		_ = c.CloseWrite()
		out, _ := io.ReadAll(io.LimitReader(c, 1<<20))
		_ = c.Close()
		t.Logf("%s -> %q", script, out)
		return string(out)
	}
	if out := run("command -v ssh; command -v rubi-ssh", ""); !strings.Contains(out, "rubi/bin/ssh") || !strings.Contains(out, "/rubi-ssh") {
		t.Errorf("ssh isn't the wrapper in PATH: %q", out)
	}
	if out := run("ssh test-target echo hi 2>&1", ""); !strings.HasPrefix(out, "hi\n") || !strings.Contains(out, "rc=0") {
		t.Errorf("echo hi: %q", out)
	}
	if out := run("ssh test-target exit 7 2>&1", ""); !strings.Contains(out, "bye") || !strings.Contains(out, "rc=7") {
		t.Errorf("exit 7: %q", out)
	}
	if out := run("ssh test-target cat 2>&1", "line one\nline two\n"); !strings.HasPrefix(out, "line one\nline two\n") || !strings.Contains(out, "rc=0") {
		t.Errorf("stdin: %q", out)
	}
	for _, c := range []string{"ssh other-target echo hi", "ssh no-such-target echo hi"} {
		out := run(c+" 2>&1", "")
		if strings.Contains(out, "hi\n") || strings.Contains(out, "rc=0") || !strings.Contains(out, "isn't available to this role") {
			t.Errorf("%s wasn't refused: %q", c, out)
		}
	}
	for _, c := range []string{"ssh -o StrictHostKeyChecking=no test-target echo hi", "ssh deploy@test-target echo hi", "ssh test-target", "ssh -i /tmp/k test-target ls"} {
		if out := run(c+" 2>&1", ""); strings.Contains(out, "hi\n") || !strings.Contains(out, "rc=2") {
			t.Errorf("%s wasn't refused by the wrapper: %q", c, out)
		}
	}
	// The VM holds no key and no host: nothing of the target in the VM's files or environment.
	if out := run("env; ls -la /tmp", ""); strings.Contains(out, "id_") || strings.Contains(out, "agent-ssh") {
		t.Errorf("key material in the VM: %q", out)
	}
	if got := len(srv.Commands()); got != 3 {
		t.Errorf("the server ran %d commands, want 3 (echo hi, exit 7, cat): %v", got, srv.Commands())
	}
}
