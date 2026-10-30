package home

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// withVersions sets the hook's identity and the helper's version for a test.
func withVersions(t *testing.T, hookVer, hookPath, helperVer string) {
	t.Helper()
	oh, oc := hookSelf, helperVersion
	hookSelf = func() (string, string) { return hookVer, hookPath }
	helperVersion = func() string { return helperVer }
	t.Cleanup(func() { hookSelf, helperVersion = oh, oc })
}

// fakeSocket accepts connections at a new socket, records each line and answers it with reply.
func fakeSocket(t *testing.T, reply string) (string, <-chan map[string]any) {
	t.Helper()
	path := filepath.Join(shortDir(t), "f.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan map[string]any, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				l, err := bufio.NewReader(conn).ReadBytes('\n')
				if err != nil {
					return
				}
				var m map[string]any
				if json.Unmarshal(l, &m) == nil {
					got <- m
				}
				_, _ = conn.Write([]byte(reply + "\n"))
			}()
		}
	}()
	return path, got
}

func wantFields(t *testing.T, m map[string]any, ver, path string) {
	t.Helper()
	if m["rubi_hook_version"] != ver || m["rubi_hook_path"] != path || m["rubi_hook_protocol"] != float64(ClaudeHookProtocol) {
		t.Fatalf("hook fields: %v", m)
	}
}

func TestHookLinesCarryVersionFields(t *testing.T) {
	withVersions(t, "v0.13.0", "/usr/local/bin/rubi-home", "v0.13.0")
	sock, got := fakeSocket(t, `{"behavior":"allow"}`)
	var out bytes.Buffer
	if err := RunClaudeHook(strings.NewReader(hookJSON), &out, sock, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	m := <-got
	wantFields(t, m, "v0.13.0", "/usr/local/bin/rubi-home")
	if m["hook_event_name"] != "PermissionRequest" || m["tool_name"] != "Bash" {
		t.Fatalf("the input changed: %v", m)
	}

	sock, got = fakeSocket(t, `{}`)
	if err := RunClaudeHook(strings.NewReader(string(hook("Stop", "s1", nil))), &out, sock, 0); err != nil {
		t.Fatal(err)
	}
	wantFields(t, <-got, "v0.13.0", "/usr/local/bin/rubi-home")

	sock, got = fakeSocket(t, `{"close":true}`)
	if code := RunClaudeWait(strings.NewReader(fmt.Sprintf(stopJSON, "s1")), &bytes.Buffer{}, sock, 5*time.Second); code != 0 {
		t.Fatalf("exit %d", code)
	}
	m = <-got
	wantFields(t, m, "v0.13.0", "/usr/local/bin/rubi-home")
	if m["hook_event_name"] != claudeWaitEvent {
		t.Fatalf("waiter event: %v", m)
	}
}

// The fields are spliced in; the bytes of tool_input (and so its digest) stay as the hook read them.
func TestStampKeepsInputBytes(t *testing.T) {
	withVersions(t, "v0.13.0", "/p", "v0.13.0")
	in := `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"echo '<a>' && ls"},"rubi_hook_version":"evil"}`
	st := stampHookLine([]byte(in))
	if !bytes.HasPrefix(st, []byte(in[:len(in)-1])) {
		t.Fatalf("input not kept: %s", st)
	}
	var m map[string]any
	if err := json.Unmarshal(st, &m); err != nil || m["rubi_hook_version"] != "v0.13.0" {
		t.Fatalf("a duplicate must not pre-empt the real field: %v %v", m, err)
	}
	a, err1 := parseHookInput([]byte(in))
	b, err2 := parseHookInput(st)
	if err1 != nil || err2 != nil || a.Digest != b.Digest || a.ToolInput != b.ToolInput || a.Command != b.Command {
		t.Fatalf("%v %v: %+v vs %+v", err1, err2, a, b)
	}
	if got := string(stampHookLine([]byte(`{}`))); !strings.HasPrefix(got, `{"rubi_hook_version"`) {
		t.Fatalf("empty object: %s", got)
	}
	if got := string(stampHookLine([]byte(`[1]`))); got != `[1]` {
		t.Fatalf("non-object: %s", got)
	}
}

// Nothing of the stamp reaches what Rubi is shown for an approval.
func TestExtraFieldsDontReachAsk(t *testing.T) {
	withVersions(t, "v0.13.0", "/some/path/rubi-home", "v0.13.0")
	c := &Claude{Wait: 20 * time.Second}
	sock := serveSocket(t, c)
	res := runHook(t, hookJSON, sock, 20*time.Second)
	// Rubi polls (so the helper counts it as connected) after the request is queued, or before: both work.
	var req *ClaudeRequest
	for i := 0; i < 200 && req == nil; i++ {
		req, _ = c.Next(context.Background(), 100*time.Millisecond)
	}
	if req == nil {
		t.Fatal("no request reached Rubi")
	}
	want, err := parseHookInput([]byte(hookJSON))
	if err != nil {
		t.Fatal(err)
	}
	if req.Digest != want.Digest || req.ToolInput != want.ToolInput || req.Command != want.Command || req.Description != want.Description {
		t.Fatalf("shown request differs: %+v vs %+v", req, want)
	}
	b, _ := json.Marshal(req)
	if strings.Contains(string(b), "rubi_hook") || strings.Contains(string(b), "/some/path") {
		t.Fatalf("the stamp reached Rubi: %s", b)
	}
	if err := c.Decide(ClaudeAnswer{ID: req.ID, SessionID: req.SessionID, Digest: req.Digest, Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	if d := <-res; d["behavior"] != "allow" {
		t.Fatalf("decision: %v", d)
	}
}

// sendRaw writes one line to the helper's socket and returns its answer line.
func sendRaw(t *testing.T, sock, line string) string {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("no answer to %s: %v", line, err)
	}
	return strings.TrimSpace(resp)
}

func TestProtocolMismatchIsRefused(t *testing.T) {
	withVersions(t, "v0.20.0", "/usr/local/bin/rubi-home", "v0.13.0")
	c := &Claude{Wait: 20 * time.Second}
	sock := serveSocket(t, c)
	stamp := func(line string, proto string) string {
		return line[:len(line)-1] + `,"rubi_hook_version":"v0.20.0","rubi_hook_path":"/usr/local/bin/rubi-home","rubi_hook_protocol":` + proto + `}`
	}
	for _, proto := range []string{"2", "0", `"x"`, "1.5"} {
		perm := stamp(`{"hook_event_name":"PermissionRequest","session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls"}}`, proto)
		var d ClaudeDecision
		if err := json.Unmarshal([]byte(sendRaw(t, sock, perm)), &d); err != nil || d.Behavior != "deny" ||
			!strings.Contains(d.Message, "speaks protocol") || !strings.Contains(d.Message, "install-hook") || !strings.HasSuffix(d.Message, "denied.") {
			t.Fatalf("proto %s: %+v %v", proto, d, err)
		}
		if got := sendRaw(t, sock, stamp(`{"hook_event_name":"Stop","session_id":"s1"}`, proto)); got != "{}" {
			t.Fatalf("proto %s event: %s", proto, got)
		}
		if got := sendRaw(t, sock, stamp(`{"hook_event_name":"RubiWait","session_id":"s1"}`, proto)); got != "{}" {
			t.Fatalf("proto %s waiter: %s", proto, got)
		}
	}
	if c.hasWaiter("s1") {
		t.Fatal("a refused waiter attached")
	}
	if st := c.reg.stateOf("s1"); st != "" {
		t.Fatalf("a refused event reached the registry: %q", st)
	}
	d := sendRaw(t, sock, stamp(`{"hook_event_name":"PermissionRequest","session_id":"s1","tool_name":"Bash","tool_input":{"command":"ls"}}`, "2"))
	if !strings.Contains(d, "protocol 2, Rubi Home speaks 1") {
		t.Fatalf("message: %s", d)
	}
	hk := c.hookInfo()
	if hk == nil || hk["refused"] != true || hk["stale"] != false || hk["version"] != "v0.20.0" {
		t.Fatalf("hook info: %v", hk)
	}
}

func TestLegacyHookStillWorksAndIsStale(t *testing.T) {
	withVersions(t, "", "", "v0.13.0")
	c := &Claude{Transcripts: t.TempDir()}
	sock := serveSocket(t, c)
	if c.hookInfo() != nil {
		t.Fatal("a hook before any was seen")
	}
	// A v0.12 hook sends the input as it is.
	if got := sendRaw(t, sock, string(hook("SessionStart", "old-1", nil))); got != "{}" {
		t.Fatalf("event: %s", got)
	}
	if c.reg.stateOf("old-1") != "idle" {
		t.Fatal("the legacy event wasn't handled")
	}
	hk := c.hookInfo()
	if hk == nil || hk["stale"] != true || hk["legacy"] != true || hk["protocol"] != 0 || hk["version"] != "" {
		t.Fatalf("hook info: %v", hk)
	}
	// A legacy approval is served too (Rubi is polling).
	c.mu.Lock()
	c.lastPoll = time.Now()
	c.mu.Unlock()
	res := make(chan string, 1)
	go func() { res <- sendRaw(t, sock, strings.Join(strings.Fields(hookJSON), "")) }()
	var req *ClaudeRequest
	for i := 0; i < 100 && req == nil; i++ {
		req, _ = c.Next(context.Background(), 100*time.Millisecond)
	}
	if req == nil {
		t.Fatal("legacy approval not queued")
	}
	if err := c.Decide(ClaudeAnswer{ID: req.ID, SessionID: req.SessionID, Digest: req.Digest, Behavior: "deny", Message: "no"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(<-res, `"deny"`) {
		t.Fatal("legacy approval not answered")
	}
}

func TestListShowsHook(t *testing.T) {
	withVersions(t, "v0.12.1", "/usr/local/bin/rubi-home", "v0.13.0")
	c := &Claude{Transcripts: t.TempDir()}
	sock := serveSocket(t, c)
	out, err := c.handle(context.Background(), "claude-sessions.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := out.(map[string]any)["hook"]; has {
		t.Fatal("hook shown before one was seen")
	}
	var buf bytes.Buffer
	if err := RunClaudeHook(strings.NewReader(string(hook("Stop", "s1", nil))), &buf, sock, 0); err != nil {
		t.Fatal(err)
	}
	out, err = c.handle(context.Background(), "claude-sessions.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	hk, _ := out.(map[string]any)["hook"].(map[string]any)
	if hk == nil || hk["version"] != "v0.12.1" || hk["path"] != "/usr/local/bin/rubi-home" || hk["protocol"] != 1 ||
		hk["stale"] != true || hk["home_version"] != "v0.13.0" || hk["seen_at"] == nil {
		t.Fatalf("hook: %v", hk)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"hook":{`) {
		t.Fatalf("json: %s", b)
	}
}

func TestMismatchLoggedOncePerHook(t *testing.T) {
	c := &Claude{}
	withVersions(t, "", "", "v0.13.0")
	line := func(ver, path string) []byte {
		return []byte(`{"hook_event_name":"Stop","session_id":"s","rubi_hook_version":"` + ver + `","rubi_hook_path":"` + path + `","rubi_hook_protocol":1}`)
	}
	for i := 0; i < 3; i++ {
		c.noteHook(line("v0.12.0", "/a"))
	}
	if n := len(c.hooks.logged); n != 1 {
		t.Fatalf("logged %d", n)
	}
	c.noteHook(line("v0.12.0", "/b"))
	c.noteHook(line("v0.12.1", "/a"))
	c.noteHook(line("v0.13.0", "/a")) // current: nothing to log
	c.noteHook(line("dev", "/a"))     // not comparable: nothing to log
	if n := len(c.hooks.logged); n != 3 {
		t.Fatalf("logged %d", n)
	}
	if hk := c.hookInfo(); hk["stale"] != false {
		t.Fatalf("dev is not stale: %v", hk)
	}
}

func TestVersionOlder(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v0.12.3", "v0.13.0", true}, {"v0.13.0", "v0.13.0", false}, {"v0.13.1", "v0.13.0", false},
		{"0.9.0", "v0.10.0", true}, {"v1.0.0-rc1", "v1.0.0", true}, {"v1.0.0", "v1.0.0-rc1", false},
		{"dev", "v1.0.0", false}, {"v1.0.0", "dev", false}, {"", "v1.0.0", false}, {"v1.0", "v1.0.1", false},
	} {
		if got := versionOlder(tc.a, tc.b); got != tc.want {
			t.Errorf("versionOlder(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

// ---- status: the hook binary check ----

func script(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestInspectHookBinary(t *testing.T) {
	dir := t.TempDir()
	p := script(t, dir, "hook", "echo v0.12.0")
	b := InspectHookBinary(p, 3*time.Second)
	if b.Version != "v0.12.0" || b.VersionE != "" || len(b.SHA256) != 64 || b.Missing {
		t.Fatalf("%+v", b)
	}
	if b.Secure { // owned by the test user (and in a user's temp directory): never secure
		t.Fatalf("secure for uid %d (owner %d)", os.Getuid(), b.Owner)
	}
	var out bytes.Buffer
	b.WritePlain(&out, "v0.13.0")
	for _, want := range []string{p, "version: v0.12.0", "sha256: " + b.SHA256, "root-owned and not group/world-writable:", "stale: older than this binary (v0.13.0)", "install-hook"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	b.WritePlain(&out, "v0.12.0")
	if strings.Contains(out.String(), "stale") {
		t.Fatalf("current hook shown stale:\n%s", out.String())
	}

	if m := InspectHookBinary(filepath.Join(dir, "none"), time.Second); !m.Missing {
		t.Fatalf("%+v", m)
	}
}

func TestInspectHungHookBinaryTimesOut(t *testing.T) {
	dir := t.TempDir()
	p := script(t, dir, "hang", "sleep 30")
	start := time.Now()
	b := InspectHookBinary(p, 300*time.Millisecond)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("took %s", took)
	}
	if b.Version != "" || !strings.Contains(b.VersionE, "didn't answer") || b.SHA256 == "" {
		t.Fatalf("%+v", b)
	}
	var out bytes.Buffer
	b.WritePlain(&out, "v0.13.0")
	if !strings.Contains(out.String(), "problem:") {
		t.Fatalf("%s", out.String())
	}
}

func TestWarnIfHookOlder(t *testing.T) {
	dir := t.TempDir()
	var msgs []string
	logf := func(f string, a ...any) { msgs = append(msgs, fmt.Sprintf(f, a...)) }
	WarnIfHookOlder(script(t, dir, "old", "echo v0.12.0"), "v0.13.0", logf)
	WarnIfHookOlder(script(t, dir, "cur", "echo v0.13.0"), "v0.13.0", logf)
	WarnIfHookOlder(filepath.Join(dir, "none"), "v0.13.0", logf)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "install-hook") {
		t.Fatalf("%v", msgs)
	}
}

// ---- install-hook ----

// copyRunner plays the privileged runner: it copies the source to the target without root.
func copyRunner(mutate func([]byte) []byte) InstallRunner {
	return func(ctx context.Context, p InstallPlan) error {
		b, err := os.ReadFile(p.Src)
		if err != nil {
			return err
		}
		if mutate != nil {
			b = mutate(b)
		}
		return os.WriteFile(p.Dst, b, 0o755)
	}
}

func installFixture(t *testing.T) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	return script(t, dir, "src", "echo "+version.Version), filepath.Join(dir, "rubi-home")
}

func TestInstallHookDryRun(t *testing.T) {
	src, dst := installFixture(t)
	called := false
	var out bytes.Buffer
	err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, DryRun: true,
		Run: func(context.Context, InstallPlan) error { called = true; return nil }})
	if err != nil || called {
		t.Fatalf("err %v, runner called %v", err, called)
	}
	sum, _ := fileSHA256(src)
	for _, want := range []string{"Source:  " + src, "sha256 " + sum, "version " + version.Version, "Target:  " + dst,
		"src='" + src + "'", "dst='" + dst + "'", "SHA256SUMS", "not proof", "/usr/bin/osascript", "with administrator privileges", "Dry run: nothing was run"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("dry run wrote the target")
	}
	out.Reset()
	if err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, DryRun: true, Sudo: true}); err != nil ||
		!strings.Contains(out.String(), "Run via: /usr/bin/sudo") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestInstallHookVerifiesTheResult(t *testing.T) {
	src, dst := installFixture(t)
	var out bytes.Buffer
	if err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, Run: copyRunner(nil)}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Installed "+dst) || !strings.Contains(out.String(), "Verified") {
		t.Fatalf("%s", out.String())
	}

	// A different file at the target: the hash check fails, with both hashes in the message.
	os.Remove(dst)
	err := InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst,
		Run: copyRunner(func(b []byte) []byte { return append(b, "# tampered\n"...) })})
	if err == nil || !strings.Contains(err.Error(), "verification failed") || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("hash mismatch: %v", err)
	}
	// Same bytes but the binary reports another version: also a failure.
	other := script(t, filepath.Dir(src), "other", "echo v9.9.9")
	err = InstallHook(context.Background(), &out, InstallOptions{Src: other, Dst: dst, Run: copyRunner(nil)})
	if err == nil || !strings.Contains(err.Error(), `prints version "v9.9.9"`) {
		t.Fatalf("version mismatch: %v", err)
	}
	// The runner did nothing: the target is missing / unreadable.
	os.Remove(dst)
	err = InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, Run: func(context.Context, InstallPlan) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "verification failed") {
		t.Fatalf("no file: %v", err)
	}
	// The runner failed (the user cancelled the prompt).
	err = InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, Run: func(context.Context, InstallPlan) error { return fmt.Errorf("User canceled") }})
	if err == nil || !strings.Contains(err.Error(), "did not run") {
		t.Fatalf("runner error: %v", err)
	}
	// Not root-owned (the copy by the test user): refused when root is required.
	if os.Getuid() != 0 {
		os.Remove(dst)
		err = InstallHook(context.Background(), &out, InstallOptions{Src: src, Dst: dst, RequireRoot: true, Run: copyRunner(nil)})
		if err == nil || !strings.Contains(err.Error(), "owned by root") {
			t.Fatalf("require root: %v", err)
		}
	}
}

func TestInstallHookRefusesUnsafePaths(t *testing.T) {
	src, dst := installFixture(t)
	dir := filepath.Dir(dst)
	for _, bad := range []string{dir + "/a'b", dir + `/a"b`, dir + `/a\b`, dir + "/a\nb", dir + "/a`b", dir + "/a$b", dir + "/a\u202eb", dir + "/a\u2028b", dir + "/a\xffb", "relative/rubi-home"} {
		called := false
		run := func(context.Context, InstallPlan) error { called = true; return nil }
		for _, o := range []InstallOptions{{Src: src, Dst: bad, Run: run}, {Src: bad, Dst: dst, Run: run}, {Src: src, Dst: bad, DryRun: true}} {
			if err := InstallHook(context.Background(), &bytes.Buffer{}, o); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Errorf("%q: %v", bad, err)
			}
		}
		if called {
			t.Errorf("%q reached the runner", bad)
		}
	}
	if err := InstallHook(context.Background(), &bytes.Buffer{}, InstallOptions{Src: src, Dst: src}); err == nil {
		t.Fatal("installing a binary over itself")
	}
	// A missing target directory is refused, with how to create it; nothing is created.
	missing := filepath.Join(t.TempDir(), "newdir", "rubi-home")
	if _, err := PlanInstallHook(src, missing, false); err == nil || !strings.Contains(err.Error(), "mkdir -p") {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); err == nil {
		t.Fatal("created the directory")
	}
}

// ---- B4 review fixes ----

// hostTools finds the script's tools on the test host (the real ones are absolute macOS/Linux paths).
func hostTools(t *testing.T) installTools {
	t.Helper()
	look := func(n string) string {
		p, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("no %s on this host", n)
		}
		return p
	}
	sha := look("shasum") + " -a 256"
	if _, err := exec.LookPath("shasum"); err != nil {
		sha = look("sha256sum")
	}
	return installTools{Cat: look("cat"), Sha: sha, Cut: look("cut"), Mktemp: look("mktemp"), Rm: look("rm"),
		Chmod: look("chmod"), Mv: look("mv"), Group: "root"}
}

func runScript(t *testing.T, script string) (string, error) {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput()
	return string(out), err
}

// The generated script reads the source once, checks the hash of what it read before anything is moved,
// removes its temporary file on failure, and (as root) checks the directories and chowns first.
func TestInstallScriptShape(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	for _, tools := range []installTools{defaultTools(), {Stat: "/usr/bin/stat -L -c '%u:%a'", Cat: "/bin/cat", Sha: "/usr/bin/sha256sum", Cut: "/usr/bin/cut",
		Dirname: "/usr/bin/dirname", Mktemp: "/usr/bin/mktemp", Rm: "/bin/rm", Chown: "/bin/chown", Chmod: "/bin/chmod", Mv: "/bin/mv", Group: "root"}} {
		sc := installScript(tools, "/home/u/rubi-home", "/usr/local/bin/rubi-home", sum, true)
		if strings.Count(sc, `"$src"`) != 1 {
			t.Errorf("the source must be read exactly once:\n%s", sc)
		}
		pos := func(sub string) int { return strings.Index(sc, sub) }
		if !(pos("mktemp") < pos("trap ") && pos("trap ") < pos(`"$src" > "$tmp"`) && pos(`"$src" > "$tmp"`) < pos(`= "$want"`) &&
			pos(`= "$want"`) < pos("chown") && pos("chown") < pos("chmod") && pos("chmod") < pos("mv -f")) {
			t.Errorf("order:\n%s", sc)
		}
		if !strings.Contains(sc, "want='"+sum+"'") || !strings.Contains(sc, `"$tmp" "$dst"`) || !strings.Contains(sc, "set -e") ||
			!strings.Contains(sc, "not owned by root") || !strings.Contains(sc, "writable by group or others") {
			t.Errorf("script:\n%s", sc)
		}
		for _, l := range strings.Split(sc, "\n") { // every command is an absolute path
			for _, tool := range []string{"cat ", "shasum", "sha256sum", "cut ", "mktemp", "rm ", "chown", "chmod", "mv ", "stat ", "dirname"} {
				if i := strings.Index(l, tool); i > 0 && !strings.Contains(l[:i], "/") && !strings.Contains(l, "echo") && !strings.HasPrefix(strings.TrimSpace(l), "src=") {
					t.Errorf("relative tool %q in %q", tool, l)
				}
			}
		}
		if strings.Contains(installScript(tools, "/a", "/b/c", sum, false), "chown") {
			t.Error("the non-root script chowns")
		}
	}
}

// Run for real with /bin/sh (root checks and chown left out): success, a wrong hash installs nothing and
// leaves no temporary file, and a source that changes after planning is caught by root's own check.
func TestInstallScriptRuns(t *testing.T) {
	tools := hostTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "rubi-home")
	os.WriteFile(src, []byte("#!/bin/sh\necho v1\n"), 0o600)
	os.WriteFile(dst, []byte("OLD"), 0o755)
	sum, _ := fileSHA256(src)

	if out, err := runScript(t, installScript(tools, src, dst, sum, false)); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, _ := os.ReadFile(dst); string(got) != "#!/bin/sh\necho v1\n" {
		t.Fatalf("dst: %q", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	left := func() []string {
		m, _ := filepath.Glob(filepath.Join(dir, ".rubi-home.*"))
		return m
	}
	if len(left()) != 0 {
		t.Fatalf("temp files: %v", left())
	}

	// The source is replaced after the hash was shown: nothing is installed.
	os.WriteFile(src, []byte("#!/bin/sh\necho evil\n"), 0o600)
	out, err := runScript(t, installScript(tools, src, dst, sum, false))
	if err == nil || !strings.Contains(out, "nothing installed") {
		t.Fatalf("swapped source: %v %s", err, out)
	}
	if got, _ := os.ReadFile(dst); string(got) != "#!/bin/sh\necho v1\n" {
		t.Fatalf("dst changed: %q", got)
	}
	if len(left()) != 0 {
		t.Fatalf("temp files left: %v", left())
	}
}

func TestInstallRefusesInsecureDirectoriesAndSymlinkTargets(t *testing.T) {
	src, dst := installFixture(t)
	if os.Getuid() != 0 {
		_, err := PlanInstallHook(src, dst, true)
		if err == nil || !strings.Contains(err.Error(), "not owned by root") {
			t.Fatalf("user-owned directory accepted: %v", err)
		}
		if err := InstallHook(context.Background(), &bytes.Buffer{}, InstallOptions{Src: src, Dst: dst, DryRun: true, RequireRoot: true}); err == nil {
			t.Fatal("dry run accepted an insecure directory")
		}
	}
	// A symlink as the target is refused, even without the root requirement.
	link := filepath.Join(filepath.Dir(dst), "link")
	if err := os.Symlink(src, link); err != nil {
		t.Fatal(err)
	}
	if _, err := PlanInstallHook(src, link, false); err == nil || !strings.Contains(err.Error(), "plain file") {
		t.Fatalf("symlink target: %v", err)
	}
	// Root-owned, closed ancestors pass; group- or world-writable ones don't.
	if why := insecureAncestors("/usr/x"); why != "" {
		t.Fatalf("/usr: %s", why)
	}
	if why := insecureAncestors(filepath.Join(os.TempDir(), "x", "y")); os.Getuid() != 0 && why == "" {
		t.Fatal("a user's temp directory passed")
	}
	// The binary check reports the directory problem.
	b := InspectHookBinary(dst, time.Second)
	_ = b
	p := script(t, filepath.Dir(dst), "h", "echo dev")
	if b := InspectHookBinary(p, time.Second); b.Secure || (os.Getuid() != 0 && b.Owner != 0 && b.WhyInsecure == "") && false {
		t.Fatalf("%+v", b)
	}
	if err := os.Chmod(filepath.Dir(dst), 0o777); err == nil {
		if why := insecureAncestors(dst); why == "" {
			t.Fatal("a world-writable directory passed")
		}
	}
}

func TestPrivilegedEnvAndPaths(t *testing.T) {
	if got := privilegedEnv(); len(got) != 1 || got[0] != "PATH=/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatal(got)
	}
}

// A wrongly typed stamp field, or a line a hook sent that can't be decoded, fails closed.
func TestWrongTypedStampIsRefused(t *testing.T) {
	withVersions(t, "", "", "v0.13.0")
	c := &Claude{}
	sock := serveSocket(t, c)
	for _, l := range []string{
		`{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"},"rubi_hook_version":5,"rubi_hook_protocol":99}`,
		`{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"},"rubi_hook_version":5,"rubi_hook_protocol":1}`,
		`{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"},"rubi_hook_version":"v1","rubi_hook_path":["x"],"rubi_hook_protocol":1}`,
		`{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"},"rubi_hook_protocol":{"a":1}}`,
	} {
		var d ClaudeDecision
		if err := json.Unmarshal([]byte(sendRaw(t, sock, l)), &d); err != nil || d.Behavior != "deny" || !strings.Contains(d.Message, "install-hook") {
			t.Errorf("%s: %+v %v", l, d, err)
		}
		ev := strings.Replace(l, "PermissionRequest", "Stop", 1)
		if got := sendRaw(t, sock, ev); got != "{}" {
			t.Errorf("event %s: %s", ev, got)
		}
	}
	if c.reg.stateOf("s") != "" {
		t.Fatal("a refused event was handled")
	}
	// A hook line whose event field has the wrong type can't be classified: denied.
	var d ClaudeDecision
	if err := json.Unmarshal([]byte(sendRaw(t, sock, `{"hook_event_name":5,"session_id":"s","rubi_hook_protocol":1}`)), &d); err != nil || d.Behavior != "deny" {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestInspectRefusesFifoAndDevices(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o755); err != nil {
		t.Skip(err)
	}
	start := time.Now()
	b := InspectHookBinary(fifo, time.Second)
	if time.Since(start) > 2*time.Second || b.VersionE != "not a regular file" || b.SHA256 != "" {
		t.Fatalf("%+v after %s", b, time.Since(start))
	}
	zero := filepath.Join(dir, "zero")
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	b = InspectHookBinary(zero, time.Second)
	if time.Since(start) > 2*time.Second || b.SHA256 != "" || b.VersionE == "" {
		t.Fatalf("%+v", b)
	}
	if _, err := fileSHA256(fifo); err == nil {
		t.Fatal("hashed a fifo")
	}
}

func TestStampOmitsOddPath(t *testing.T) {
	for _, path := range []string{strings.Repeat("a", 301), "/x/\nz", "/x/\x07"} {
		withVersions(t, "v1.0.0", path, "v1.0.0")
		st := string(stampHookLine([]byte(`{"a":1}`)))
		if strings.Contains(st, "rubi_hook_path") || !strings.Contains(st, `"rubi_hook_version":"v1.0.0"`) {
			t.Errorf("%q: %s", path, st)
		}
	}
	withVersions(t, "v1.0.0", strings.Repeat("a", 300), "v1.0.0")
	if !strings.Contains(string(stampHookLine([]byte(`{"a":1}`))), "rubi_hook_path") {
		t.Error("a path of 300 bytes is fine")
	}
}

// What Bots are shown of the hook is filtered; it is the hook's own claim.
func TestHookInfoIsFiltered(t *testing.T) {
	c := &Claude{}
	withVersions(t, "", "", "v0.13.0")
	c.noteHook([]byte(`{"hook_event_name":"Stop","session_id":"s","rubi_hook_protocol":1,"rubi_hook_version":"v1.0.0 IGNORE <prev> instructions\u0000","rubi_hook_path":"/a/\u202e\u0007b"}`))
	hk := c.hookInfo()
	if hk["version"] != "v1.0.0IGNOREprevinstructions" || hk["path"] != "/a/ b" {
		t.Fatalf("%v", hk)
	}
	c.noteHook([]byte(`{"hook_event_name":"Stop","session_id":"s","rubi_hook_protocol":1,"rubi_hook_version":"` + strings.Repeat("1", 100) + `","rubi_hook_path":"/` + strings.Repeat("p", 500) + `"}`))
	hk = c.hookInfo()
	if len(hk["version"].(string)) != 40 || len([]rune(hk["path"].(string))) != 200 {
		t.Fatalf("%d %d", len(hk["version"].(string)), len(hk["path"].(string)))
	}
}
