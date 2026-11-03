package t3vm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

const (
	shippedURL     = `"http://127.0.0.1:3130/ask"`
	shippedTimeout = `const TIMEOUT_MS = 295000;`
)

// hookRun is what a run of the hook script left: what it printed, its exit status and its standard error.
type hookRun struct {
	out    hookOut
	raw    string
	code   int
	stderr string
}

// runHook runs a copy of the embedded script under node with the hook input on stdin. The shipped script has its
// address and timeout fixed, so the copy has them replaced (the test fails if the shipped text changes shape).
// extraEnv is added to the environment, nodeArgs go before the script.
func runHook(t *testing.T, gateURL, stdin string, timeoutMS string, extraEnv []string, nodeArgs ...string) hookRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	src := string(HookScript)
	if strings.Count(src, shippedURL) != 1 || strings.Count(src, shippedTimeout) != 1 {
		t.Fatal("the script's address and timeout constants changed shape")
	}
	src = strings.Replace(src, shippedURL, `"`+gateURL+`"`, 1)
	src = strings.Replace(src, shippedTimeout, "const TIMEOUT_MS = "+timeoutMS+";", 1)
	script := filepath.Join(t.TempDir(), "rubi-hook.mjs")
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, append(nodeArgs, script)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the hook script did not finish")
	}
	code := 0
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatal(runErr)
	}
	r := hookRun{raw: out.String(), code: code, stderr: errb.String()}
	if err := json.Unmarshal(out.Bytes(), &r.out); err != nil {
		t.Fatalf("the hook printed %q: %v", out.String(), err)
	}
	return r
}

// wantBlocked: the deny JSON on stdout and exit status 2, which Claude Code treats as a block (exit 1 would let the
// tool run).
func wantBlocked(t *testing.T, r hookRun, reason string) {
	t.Helper()
	h := r.out.HookSpecificOutput
	if h == nil || h.HookEventName != "PreToolUse" || h.PermissionDecision != "deny" || !strings.Contains(h.PermissionDecisionReason, reason) {
		t.Fatalf("not a deny with %q: %q", reason, r.raw)
	}
	if r.code != 2 || !strings.Contains(r.stderr, reason) {
		t.Fatalf("exit %d, stderr %q: a failure must exit 2 and say why", r.code, r.stderr)
	}
}

func TestHookScriptPostsTheInputAndPrintsTheGatesAnswer(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.Store(r.Method + " " + r.URL.Path + " " + string(b))
		w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"from the gate"}}`))
	}))
	defer srv.Close()
	r := runHook(t, srv.URL+"/ask", goodInput, "5000", nil)
	// the gate's own deny is printed as it is and exits 0 (Claude Code blocks the tool on the JSON)
	if r.out.HookSpecificOutput == nil || r.out.HookSpecificOutput.PermissionDecisionReason != "from the gate" || r.code != 0 {
		t.Fatalf("%q exit %d", r.raw, r.code)
	}
	if got.Load() != "POST /ask "+goodInput {
		t.Fatalf("the gate got %q", got.Load())
	}
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv2.Close()
	if r := runHook(t, srv2.URL+"/ask", goodInput, "5000", nil); r.out.HookSpecificOutput != nil || r.code != 0 {
		t.Fatalf("an allow turned into %q exit %d", r.raw, r.code)
	}
}

func TestHookScriptBlocksWhenTheGateFails(t *testing.T) {
	status := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }))
	defer status.Close()
	wantBlocked(t, runHook(t, status.URL+"/ask", goodInput, "5000", nil), "answered 500")

	for _, body := range []string{`allow`, `[]`, `null`, ``} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		wantBlocked(t, runHook(t, srv.URL+"/ask", goodInput, "5000", nil), "not")
		srv.Close()
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close() // nothing listens any more
	wantBlocked(t, runHook(t, url+"/ask", goodInput, "5000", nil), "unreachable")

	hang := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer slow.Close()
	defer close(hang)
	wantBlocked(t, runHook(t, slow.URL+"/ask", goodInput, "300", nil), "unreachable")

	// a gate that redirects is not followed
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{}`)) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	wantBlocked(t, runHook(t, redir.URL+"/ask", goodInput, "5000", nil), "unreachable")
	if hits.Load() != 0 {
		t.Error("the redirect was followed")
	}
}

// A crash of the script is a block too: Claude Code lets the tool run on exit status 1 or any other status but 2.
func TestHookScriptBlocksWhenItCrashes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // the crash comes while the request is out
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"exception": `setTimeout(() => { throw new Error("crash in the hook"); }, 50);`,
		"rejection": `setTimeout(() => { Promise.reject(new Error("crash in the hook")); }, 50);`,
	} {
		pre := filepath.Join(dir, name+".cjs")
		if err := os.WriteFile(pre, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		wantBlocked(t, runHook(t, srv.URL+"/ask", goodInput, "5000", nil, "--require", pre), "crash in the hook")
	}
}

// A repository's settings can put variables into the hook's environment; none of them may steer the hook.
func TestHookScriptIgnoresItsEnvironment(t *testing.T) {
	if bytes.Contains(HookScript, []byte("process.env")) {
		t.Fatal("the script reads the environment")
	}
	var hits atomic.Int32
	allowAll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{}`)) }))
	defer allowAll.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	env := []string{"RUBI_HOOK_URL=" + allowAll.URL + "/ask", "RUBI_HOOK_TIMEOUT_MS=1", "RUBI_HOOK_ADDR=data:application/json,{}", "NODE_OPTIONS="}
	wantBlocked(t, runHook(t, url+"/ask", goodInput, "5000", env), "unreachable")
	if hits.Load() != 0 {
		t.Fatal("the hook went to the address in its environment")
	}
	// and with a data: URL in the environment the fixed address is still the one used
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"fixed"}}`))
	}))
	defer srv.Close()
	r := runHook(t, srv.URL+"/ask", goodInput, "5000", []string{"RUBI_HOOK_URL=data:application/json,{}"})
	if hits.Load() != 1 || r.out.HookSpecificOutput == nil || r.out.HookSpecificOutput.PermissionDecisionReason != "fixed" {
		t.Fatalf("hits %d, %q", hits.Load(), r.raw)
	}
}

// The script only talks to http://127.0.0.1: anything else in its constant is refused before a request is made.
func TestHookScriptRefusesAnAddressThatIsNotLoopbackHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte(`{}`)) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	for _, u := range []string{"http://localhost:" + port + "/ask", "http://[::1]:" + port + "/ask", "https://127.0.0.1:" + port + "/ask", "data:application/json,{}", "http://192.168.64.1:" + port + "/ask"} {
		wantBlocked(t, runHook(t, u, goodInput, "5000", nil), "misconfigured")
	}
	if hits.Load() != 0 {
		t.Fatal("a request was made")
	}
}

// The script and the gate together: what the gate decides is what the hook prints.
func TestHookScriptAgainstTheGate(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "deny", Message: "no way"}}
	srv := httptest.NewServer(testGate(f))
	defer srv.Close()
	r := runHook(t, srv.URL+"/ask", goodInput, "5000", nil)
	if r.out.HookSpecificOutput == nil || !strings.Contains(r.out.HookSpecificOutput.PermissionDecisionReason, "no way") || r.out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("%q", r.raw)
	}
	f.res = agent.AskResult{Behavior: "allow"}
	if r := runHook(t, srv.URL+"/ask", goodInput, "5000", nil); r.out.HookSpecificOutput != nil || r.code != 0 {
		t.Fatalf("allow printed %q exit %d", r.raw, r.code)
	}
	if r := runHook(t, srv.URL+"/ask", "not json at all", "5000", nil); r.out.HookSpecificOutput == nil || r.out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("malformed input: %q", r.raw)
	}
	if f.count() != 2 {
		t.Fatalf("asked %d times", f.count())
	}
}
