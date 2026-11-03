package home

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// TestLiveT3LoginReachesThePrompt runs the real claude of the runner image in the sign-in container, up to its paste
// prompt: Home must report the sign-in link and that it waits for the code. Then it cancels; no code is sent, no
// account is used, and no credential is written. Run with RUBI_T3VM_LIVE=1 (the container tool and the image are needed).
func TestLiveT3LoginReachesThePrompt(t *testing.T) {
	if os.Getenv("RUBI_T3VM_LIVE") != "1" {
		t.Skip("set RUBI_T3VM_LIVE=1 to run against the container tool and the runner image")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Fatal("RUBI_T3VM_LIVE=1 but the container tool isn't installed")
	}
	base, err := os.MkdirTemp("", "t3l")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	h, err := Open(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: tokenHash("tok")}} }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	// The image first (it may have to be built, with the new helper in it), so the sign-in's own wait is short.
	if err := (&agent.AppleContainer{EnvDir: base}).EnsureImage(ctx); err != nil {
		t.Fatalf("the runner image: %v", err)
	}
	t.Cleanup(func() { h.T3.login().Cancel() })
	if _, err := handle(t, h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		st, err := handle(t, h, "tok", "t3-setup.status", nil)
		if err != nil {
			t.Fatal(err)
		}
		step = stepByID(t, st, "login")
		if step["state"] == "failed" || step["wants_code"] == true {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if step["state"] != "working" || step["wants_code"] != true {
		t.Fatalf("the real claude did not reach its prompt: %v", step)
	}
	link, _ := step["url"].(string)
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || (u.Hostname() != "claude.com" && u.Hostname() != "claude.ai" && u.Hostname() != "platform.claude.com" && u.Hostname() != "console.anthropic.com") {
		t.Fatalf("the link: %q", link)
	}
	if !strings.Contains(link, "oauth") {
		t.Errorf("the link doesn't look like a sign-in link: %q", link)
	}
	t.Logf("link host %s, wants the code", u.Hostname())
	// cancel: the container goes, nothing is signed in
	if res, err := handle(t, h, "tok", "t3-setup.cancel", nil); err != nil || res["stopped"] != true {
		t.Fatalf("cancel: %v %v", res, err)
	}
	for i := 0; i < 60 && len(liveContainers("rubi-t3-login")) > 0; i++ {
		time.Sleep(time.Second)
	}
	if n := liveContainers("rubi-t3-login"); len(n) != 0 {
		t.Errorf("the sign-in container is still there: %v", n)
	}
	if agent.SignedInAt(agent.SignInDirOf(base)) {
		t.Error("signed in without a code")
	}
	st, _ := handle(t, h, "tok", "t3-setup.status", nil)
	if s := stepByID(t, st, "login"); s["state"] == "working" || s["url"] != nil {
		t.Errorf("after cancel: %v", s)
	}
}

func liveContainers(prefix string) []string {
	out, err := exec.Command("container", "list", "--all", "--quiet").CombinedOutput()
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Fields(string(out)) {
		if strings.HasPrefix(l, prefix) {
			names = append(names, l)
		}
	}
	return names
}

// TestLiveT3LoginHelperUnderTheRealScript runs a copy of the sign-in helper (claude's path changed to a fake claude, in a
// folder whose name has a quote and a space) in the real image, so that its `script -qfc ... stty ...` command line is
// the image's own util-linux one, with a real terminal. The fake plays `claude auth login --claudeai`. Run with
// RUBI_T3VM_LIVE=1.
func TestLiveT3LoginHelperUnderTheRealScript(t *testing.T) {
	if os.Getenv("RUBI_T3VM_LIVE") != "1" {
		t.Skip("set RUBI_T3VM_LIVE=1 to run against the container tool and the runner image")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Fatal("RUBI_T3VM_LIVE=1 but the container tool isn't installed")
	}
	base, err := os.MkdirTemp("", "t3s")
	if err != nil {
		t.Fatal(err)
	}
	if base, err = filepath.EvalSymlinks(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	ac := &agent.AppleContainer{EnvDir: base}
	if err := ac.EnsureImage(ctx); err != nil {
		t.Fatalf("the runner image: %v", err)
	}
	root := liveRepoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "runner", "container", "rubi-claude-login"))
	if err != nil {
		t.Fatal(err)
	}
	fake, err := os.ReadFile(filepath.Join(root, "runner", "test", "support", "fake-auth-login.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	const inside = "/it's a dir" // where the files are in the container
	helper := regexp.MustCompile(`(?m)^const CLAUDE = ".*";$`).ReplaceAllString(string(src), `const CLAUDE = "`+inside+`/claude";`)
	helper = regexp.MustCompile(`(?m)^const GRACE_MS = \d+;`).ReplaceAllString(helper, "const GRACE_MS = 3000;")
	if helper == string(src) || !strings.Contains(helper, inside+"/claude") {
		t.Fatal("the helper's constants were not found")
	}
	files := filepath.Join(base, "files")
	signin := filepath.Join(base, "signin")
	for _, d := range []string{files, signin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(signin, 0o777); err != nil { // the container's user is not the Mac user
		t.Fatal(err)
	}
	for name, body := range map[string]string{"helper": helper, "fake.mjs": string(fake), "claude": "#!/bin/sh\nexec node \"" + inside + "/fake.mjs\" \"$@\"\n"} {
		if err := os.WriteFile(filepath.Join(files, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	name := "rubi-logintest-" + fmt.Sprint(time.Now().UnixNano()) // not rubi-t3-*: the session server sweeps those
	t.Cleanup(func() { _ = exec.Command("container", "delete", "--force", name).Run() })
	cmd := exec.CommandContext(ctx, "container", "run", "--rm", "-i", "--name", name, "--network", "none", "--user", "10001", "--read-only", "--tmpfs", "/tmp",
		"--mount", "type=bind,source="+files+",target="+inside+",readonly", "--mount", "type=bind,source="+signin+",target="+signin,
		"-e", "CLAUDE_SECURESTORAGE_CONFIG_DIR="+signin, "-e", "FAKE_CURSOR_GAPS=1", "--entrypoint", "node", ac.ImageTag(), inside+"/helper")
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	var all []string
	next := func(want func(string) bool) {
		t.Helper()
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the helper ended; saw %v; stderr: %s", all, stderr.String())
				}
				all = append(all, l)
				if want(l) {
					return
				}
			case <-time.After(2 * time.Minute):
				t.Fatalf("no answer; saw %v; stderr: %s", all, stderr.String())
			}
		}
	}
	next(func(l string) bool { return strings.HasPrefix(l, "URL https://claude.com/") })
	next(func(l string) bool { return l == "CODE?" })
	if _, err := io.WriteString(in, "good\n"); err != nil {
		t.Fatal(err)
	}
	next(func(l string) bool { return l == "SAVED" })
	if err := cmd.Wait(); err != nil {
		t.Errorf("the helper ended badly: %v", err)
	}
	if !agent.SignedInAt(signin) {
		t.Error("the fake's credential file isn't in the sign-in folder")
	}
	if strings.Contains(strings.Join(all, "\n"), "sk-ant") || strings.Contains(strings.Join(all, "\n"), "FAKEFAKE") {
		t.Errorf("the credential leaked into the protocol: %v", all)
	}
}

func liveRepoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		if parent := filepath.Dir(dir); parent != dir {
			dir = parent
			continue
		}
		t.Fatal("go.mod not found")
	}
}
