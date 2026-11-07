package home

// The self-test of the T3 path (t3-setup.run {step: "selftest"}): one row per role. For each role it checks that T3 runs
// and answers to the bridge, that the role's instance and shim are in place, that a VM starts for the role the way a
// session's does (through the session server: `claude --version`, then a probe in the same VM spec), that the VM has no
// network of its own and reaches the outside only through the allow-list proxy, that its hook gate answers, that its
// managed settings keep the agent from the shared sign-in folder, and what the sign-in state is.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/t3vm"
	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
)

// T3StepSelfTest is the setup page's self-test row.
const T3StepSelfTest = "selftest"

// vmProbeTemplate runs in the role's VM (node -e, with the sign-in folder as its argument) and prints `key value` lines:
//
//	direct1, direct2, direct6, dns   what four attempts to leave the VM by itself came to: two public IPv4 addresses, one
//	                                 IPv6 address and a name lookup. "CONNECTED" or "ERR <code>"; the host decides which
//	                                 codes show there is no way out (a refused or reset connection shows there is one).
//	proxy, proxyhost, proxyport      CONNECT through the VM's allow-list proxy: to the Anthropic API (must work), to a host the
//	                                 proxy refuses for good (the updater's, which it refuses without asking anyone) and to a
//	                                 port that is not 443 (must both be refused with a 403)
//	hook                             the hook gate's answer to a malformed call (a deny, and it asks nobody)
//	settings                         what the managed settings configure: the managed hook only, and deny rules naming the folder
//	cred                             whether the agent user can open the shared sign-in file: READABLE, DENIED <code> or ABSENT.
//	                                 One byte is read and thrown away; the content is never looked at or printed.
//
// {{HOOK_HOST}} and {{HOOK_PORT}} are the hook gate's address (agent.PipeHookAddr).
const vmProbeTemplate = `
const net = require("net"), http = require("http"), fs = require("fs"), dns = require("dns"), path = require("path");
const signin = process.argv[1];
const KEYS = ["direct1", "direct2", "direct6", "dns", "proxy", "proxyhost", "proxyport", "hook", "settings", "cred"];
const res = {};
let left = KEYS.length;
function finish() { for (const [k, v] of Object.entries(res)) console.log(k + " " + v); process.exit(0); }
function set(k, v) { if (res[k] === undefined) { res[k] = v; if (--left === 0) finish(); } }
setTimeout(() => { for (const k of KEYS) if (res[k] === undefined) res[k] = "TIMEOUT"; finish(); }, 60000);
function direct(key, host, port) {
  const s = net.connect({ host, port });
  s.setTimeout(5000);
  s.on("connect", () => { set(key, "CONNECTED"); s.destroy(); });
  s.on("error", (e) => set(key, "ERR " + (e.code || "error")));
  s.on("timeout", () => { set(key, "ERR timeout"); s.destroy(); });
}
direct("direct1", "1.1.1.1", 443);
direct("direct2", "8.8.8.8", 443);
direct("direct6", "2606:4700:4700::1111", 443);
dns.lookup("example.com", (e, addr) => set("dns", e ? "ERR " + (e.code || "error") : "RESOLVED"));
function connectVia(key, target) {
  try {
    const u = new URL(process.env.HTTPS_PROXY);
    const auth = Buffer.from(decodeURIComponent(u.username) + ":" + decodeURIComponent(u.password)).toString("base64");
    const req = http.request({ host: u.hostname, port: u.port, method: "CONNECT", path: target, headers: { "Proxy-Authorization": "Basic " + auth }, timeout: 20000 });
    req.on("connect", (r, sock) => { set(key, String(r.statusCode)); sock.destroy(); });
    req.on("response", (r) => set(key, String(r.statusCode)));
    req.on("error", (e) => set(key, "ERR " + (e.code || "error")));
    req.on("timeout", () => { set(key, "ERR timeout"); req.destroy(); });
    req.end();
  } catch (e) { set(key, "ERR noproxy"); }
}
connectVia("proxy", "api.anthropic.com:443");
connectVia("proxyhost", "downloads.claude.ai:443");
connectVia("proxyport", "api.anthropic.com:8443");
const hr = http.request({ host: "{{HOOK_HOST}}", port: {{HOOK_PORT}}, path: "/ask", method: "POST", timeout: 10000 }, (r) => {
  let b = "";
  r.on("data", (d) => (b += d));
  r.on("end", () => { try { set("hook", JSON.parse(b).hookSpecificOutput.permissionDecision); } catch (e) { set("hook", "UNREADABLE " + r.statusCode); } });
});
hr.on("error", (e) => set("hook", "ERR " + (e.code || "error")));
hr.on("timeout", () => { set("hook", "ERR timeout"); hr.destroy(); });
hr.end("{}");
try {
  const m = JSON.parse(fs.readFileSync("/etc/claude-code/managed-settings.json", "utf8"));
  const deny = (m.permissions && m.permissions.deny) || [];
  const rules = (tool) => deny.filter((r) => r.startsWith(tool + "(") && r.includes(signin)).length;
  set("settings", "hooksOnly=" + (m.allowManagedHooksOnly === true) + " read=" + rules("Read") + " edit=" + rules("Edit") + " bash=" + rules("Bash"));
} catch (e) { set("settings", "ERR " + (e.code || "unreadable")); }
try {
  const fd = fs.openSync(path.join(signin, ".credentials.json"), "r");
  fs.readSync(fd, Buffer.alloc(1), 0, 1, 0);
  fs.closeSync(fd);
  set("cred", "READABLE");
} catch (e) { set("cred", e.code === "ENOENT" ? "ABSENT" : "DENIED " + (e.code || "error")); }
`

// vmProbeScript is the template with the hook gate's address put in.
var vmProbeScript = func() string {
	host, port, _ := strings.Cut(agent.PipeHookAddr, ":")
	return strings.NewReplacer("{{HOOK_HOST}}", host, "{{HOOK_PORT}}", port).Replace(vmProbeTemplate)
}()

// noWayOut are the errors that show a connection attempt had nowhere to go: no route, no network, no address family, no
// name service. A refused or reset connection (ECONNREFUSED, ECONNRESET) and a timeout show something answered or may
// have: they are not proof.
func noWayOut(v string) bool {
	code, ok := strings.CutPrefix(v, "ERR ")
	if !ok {
		return false
	}
	switch code {
	case "ENETUNREACH", "EHOSTUNREACH", "ENETDOWN", "EADDRNOTAVAIL", "EAFNOSUPPORT", "ENOTFOUND", "ENODATA":
		return true
	}
	return strings.HasPrefix(code, "EAI_")
}

// selfRow is the self-test's result for one role.
type selfRow struct {
	State, Reason string
	Progress      []string
}

// selfPath is the rubi-home the shims run (this binary).
func (t *T3Home) selfPath() (string, error) {
	if t.SelfPath != nil {
		return t.SelfPath()
	}
	return os.Executable()
}

func (t *T3Home) vmVersion(ctx context.Context, role string) (string, error) {
	if t.VMVersion != nil {
		return t.VMVersion(ctx, role)
	}
	s := t.h.sessionServer()
	if s == nil {
		return "", errors.New("the session server isn't running")
	}
	return s.Version(ctx, role)
}

func (t *T3Home) vmProbe(ctx context.Context, role string, cmd []string) ([]byte, error) {
	if t.VMProbe != nil {
		return t.VMProbe(ctx, role, cmd)
	}
	s := t.h.sessionServer()
	if s == nil {
		return nil, errors.New("the session server isn't running")
	}
	return s.Probe(ctx, role, cmd)
}

func parseProbe(out []byte) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			m[k] = v
		}
	}
	return m
}

// selfTestRole runs the checks for one role. It stops at the first one that fails; a missing sign-in alone is
// "needs_user", not a failure.
func (t *T3Home) selfTestRole(ctx context.Context, r agent.T3ProjectRow) selfRow {
	var lines []string
	ok := func(f string, a ...any) { lines = append(lines, "ok: "+fmt.Sprintf(f, a...)) }
	fail := func(f string, a ...any) selfRow {
		msg := fmt.Sprintf(f, a...)
		return selfRow{"failed", msg, append(lines, "FAILED: "+msg)}
	}
	cl, err := t.ready()
	if err != nil {
		return fail("%v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = cl.Projects(pctx)
	cancel()
	if err != nil {
		return fail("T3 runs but doesn't answer the bridge: %v", err)
	}
	ok("T3 is running and the bridge is paired")

	inst := "claude-" + r.Role
	found := false
	for _, i := range t.instances() {
		found = found || i == inst
	}
	if !found {
		return fail("the role's instance %s is not in T3's settings", inst)
	}
	ok("the instance %s is in T3's settings", inst)
	self, err := t.selfPath()
	if err != nil {
		return fail("can't find rubi-home: %v", err)
	}
	want, err := t3vm.ShimScript(self, t.h.Dir, r.Role)
	if err != nil {
		return fail("%v", err)
	}
	got, err := os.ReadFile(filepath.Join(t.h.Dir, "t3", "bin", inst))
	if err != nil || string(got) != string(want) {
		return fail("the shim of %s is missing or out of date (change the role again to write it)", inst)
	}
	ok("the shim of %s is in place and runs this rubi-home", inst)

	vctx, vcancel := context.WithTimeout(ctx, 3*time.Minute)
	v, err := t.vmVersion(vctx, r.Role)
	vcancel()
	if err != nil {
		return fail("the image can't tell claude's version: %v", err)
	}
	ok("claude --version through the session server: %s", clipStr(strings.TrimSpace(v), 80))

	signin, err := agent.EnsureSignInDir(t.signInDir())
	if err != nil {
		return fail("%v", err)
	}
	cctx, ccancel := context.WithTimeout(ctx, 6*time.Minute)
	out, err := t.vmProbe(cctx, r.Role, []string{"node", "-e", vmProbeScript, signin})
	ccancel()
	if err != nil {
		return fail("a VM for the role didn't start or its probe failed: %v", err)
	}
	ok("a VM for the role started the way a session's does")
	m := parseProbe(out)
	for _, k := range []string{"direct1", "direct2", "direct6", "dns"} {
		if !noWayOut(m[k]) {
			return fail("the VM may have a network of its own: the attempt %q came to %q, not to \"no route\"", k, clipStr(m[k], 40))
		}
	}
	ok("the VM has no way out of its own: two IPv4 addresses, an IPv6 address and a name lookup all find no route")
	if m["proxy"] != "200" {
		return fail("the VM's proxy didn't open api.anthropic.com (answer %q)", clipStr(m["proxy"], 40))
	}
	for _, k := range []string{"proxyhost", "proxyport"} {
		if m[k] != "403" {
			return fail("the VM's proxy did not refuse what it must refuse (%s: answer %q, not 403)", k, clipStr(m[k], 40))
		}
	}
	ok("the VM's proxy opens api.anthropic.com and refuses the updater's host and a port other than 443: the allow-list proxy")
	if m["hook"] != "deny" {
		return fail("the VM's hook gate didn't answer a malformed call with a deny (answer %q)", clipStr(m["hook"], 40))
	}
	ok("the VM's hook gate answers (and denies what it can't vouch for)")
	var read, edit, bash int
	var hooksOnly string
	if _, err := fmt.Sscanf(m["settings"], "hooksOnly=%s read=%d edit=%d bash=%d", &hooksOnly, &read, &edit, &bash); err != nil || hooksOnly != "true" || read < 1 || edit < 1 || bash < 1 {
		return fail("the VM's managed settings are not configured as expected (%q)", clipStr(m["settings"], 80))
	}
	ok("the managed settings are configured: only the managed hook, and deny rules for reading, editing and naming the sign-in folder (rules the agent's tools follow; they are not a boundary)")
	residual := false
	switch c := m["cred"]; {
	case c == "READABLE":
		residual = true
		lines = append(lines, "note: the agent can read the shared sign-in file: residual risk of decision 4 (one sign-in folder for every role)")
	case c == "ABSENT":
		ok("no sign-in file yet, so whether the agent could read it was not checked")
	case strings.HasPrefix(c, "DENIED"):
		ok("the agent user can't open the shared sign-in file (%s)", clipStr(c, 40))
	default:
		return fail("the VM's check of the sign-in file gave no answer (%q)", clipStr(c, 40))
	}

	if !agent.SignedInAt(signin) {
		lines = append(lines, "needs you: no credential file yet")
		return selfRow{"needs_user", "Everything but the sign-in works. Sign in to Claude (the login step), then run the self-test again.", lines}
	}
	ok("a credential file is present (shared by every role; whether it still works is claude's to say)")
	reason := "T3, the role's VM and its isolation work."
	if residual {
		reason += " Residual risk (decision 4): the agent can read the shared sign-in file."
	}
	return selfRow{"done", reason, lines}
}

// checkSelfTarget refuses a project or role the self-test would find nothing for.
func (t *T3Home) checkSelfTarget(project, role string) error {
	for _, r := range t.h.Agent.T3Projects() {
		if (project == "" || r.Project == project) && (role == "" || r.Role == role) {
			return nil
		}
	}
	return errors.New("no such role or project to test")
}

// runSelfTest runs the self-test for the roles of a project, or one role, or all (both empty).
func (t *T3Home) runSelfTest(ctx context.Context, project, role string) error {
	var rows []agent.T3ProjectRow
	for _, r := range t.h.Agent.T3Projects() {
		if (project == "" || r.Project == project) && (role == "" || r.Role == role) {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return errors.New("no such role or project to test")
	}
	for _, r := range rows {
		res := t.selfTestRole(ctx, r)
		for i, l := range res.Progress {
			res.Progress[i] = clipStr(l, 240)
		}
		t.mu.Lock()
		if t.selfRows == nil {
			t.selfRows = map[string]selfRow{}
		}
		t.selfRows[r.Role] = res
		t.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// selfTestStep is the "selftest" row: one row per role with the last result.
func (t *T3Home) selfTestStep() map[string]any {
	t.mu.Lock()
	job := t.job
	pending := t.pendingSelf
	results := map[string]selfRow{}
	for k, v := range t.selfRows {
		results[k] = v
	}
	paired := t.srv != nil && t.srv.Alive() && t.cl != nil
	t.mu.Unlock()
	working := job != nil && (job.Step == T3StepSelfTest || job.Step == T3StepAll)
	rows := []map[string]any{}
	agg := map[string]int{}
	for _, r := range t.h.Agent.T3Projects() {
		row := map[string]any{"id": r.Role, "dir": r.Dir, "state": "todo", "reason": "Not run yet.", "action": "selftest", "progress": []string{}}
		res, ran := results[r.Role]
		if ran {
			row["state"], row["reason"], row["progress"] = res.State, res.Reason, res.Progress
		}
		if working {
			row["state"], row["reason"] = "working", setup.SelfTestRunning
		} else if !paired && !ran {
			row["state"], row["reason"], row["action"] = "todo", "Needs T3 running and paired first.", ""
		}
		agg[row["state"].(string)]++
		rows = append(rows, row)
	}
	state, reason := "todo", "Not run yet."
	switch {
	case len(rows) == 0:
		reason = "Make a role first."
	case agg["working"] > 0:
		state, reason = "working", setup.SelfTestRunning
	case pending:
		state, reason = "todo", "Waiting for the other setup step to finish. The self-test runs then."
		for _, row := range rows {
			if row["state"] == "todo" {
				row["reason"] = reason
				row["action"] = "selftest"
			}
		}
	case agg["failed"] > 0:
		cause := ""
		if len(rows) == 1 {
			cause, _ = rows[0]["reason"].(string)
		}
		state, reason = "failed", setup.SelfTestFailedReason(agg["failed"], len(rows), cause)
	case agg["needs_user"] > 0:
		state, reason = "needs_user", "Sign in to Claude, then run it again."
	case agg["done"] == len(rows):
		state, reason = "done", "Every role passed."
	}
	return map[string]any{"id": "selftest", "title": setup.TitleSelfTest, "state": state, "reason": reason, "action": "selftest",
		"projects": rows, "progress": []string{}}
}
