package home

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

const goodProbe = "direct1 ERR ENETUNREACH\ndirect2 ERR EHOSTUNREACH\ndirect6 ERR EADDRNOTAVAIL\ndns ERR EAI_AGAIN\nproxy 200\nproxyhost 403\nproxyport 403\n" +
	"hook deny\nsettings hooksOnly=true read=2 edit=2 bash=1\ncred DENIED EACCES\n"

// vmFake is the session server's part in the self-test.
type vmFake struct {
	mu       sync.Mutex
	versions []string // roles asked
	probes   [][]string
	probeOut string
	verErr   error
	probeErr error
	gate     chan struct{}
}

func (v *vmFake) install(t *T3Home) {
	t.VMVersion = func(_ context.Context, role string) (string, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.versions = append(v.versions, role)
		return "2.1.314 (Claude Code)\n", v.verErr
	}
	t.VMProbe = func(_ context.Context, role string, cmd []string) ([]byte, error) {
		if v.gate != nil {
			<-v.gate
		}
		v.mu.Lock()
		defer v.mu.Unlock()
		v.probes = append(v.probes, append([]string{role}, cmd...))
		return []byte(v.probeOut), v.probeErr
	}
}

func selfLife(t *testing.T) (*life, *vmFake, string) {
	t.Helper()
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	l.up(t)
	vm := &vmFake{probeOut: goodProbe}
	vm.install(l.t3)
	return l, vm, role
}

func signIn(t *testing.T, l *life) {
	t.Helper()
	dir := agent.SignInDirOf(l.h.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, agent.CredentialFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runSelf(t *testing.T, l *life, args map[string]any) map[string]any {
	t.Helper()
	args["step"] = "selftest"
	if _, err := handle(t, l.h, "tok", "t3-setup.run", args); err != nil {
		t.Fatal(err)
	}
	l.t3.Wait()
	st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	return stepByID(t, st, "selftest")
}

func rowOf(t *testing.T, step map[string]any, role string) map[string]any {
	t.Helper()
	for _, r := range step["projects"].([]any) {
		if m := r.(map[string]any); m["id"] == role {
			return m
		}
	}
	t.Fatalf("no row for %s in %v", role, step)
	return nil
}

func lines(row map[string]any) string {
	var out []string
	for _, l := range row["progress"].([]any) {
		out = append(out, l.(string))
	}
	return strings.Join(out, "\n")
}

func TestT3SelfTestPassesAndReportsEachCheck(t *testing.T) {
	l, vm, role := selfLife(t)
	signIn(t, l)
	step := runSelf(t, l, map[string]any{})
	row := rowOf(t, step, role)
	if step["state"] != "done" || row["state"] != "done" || !strings.Contains(row["reason"].(string), "isolation work") {
		t.Fatalf("step %v row %v", step["state"], row)
	}
	p := lines(row)
	for _, want := range []string{"T3 is running and the bridge is paired", "the instance claude-" + role + " is in T3's settings", "shim of claude-" + role,
		"claude --version through the session server: 2.1.314 (Claude Code)", "a VM for the role started the way a session's does", "no way out of its own",
		"the allow-list proxy", "hook gate answers", "managed settings are configured", "a credential file is present", "can't open the shared sign-in file"} {
		if !strings.Contains(p, want) {
			t.Errorf("progress lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "FAILED") {
		t.Errorf("a failure line in a pass:\n%s", p)
	}
	// the probe is the node script, with the sign-in folder, in the role's VM; the version was asked first
	if len(vm.versions) != 1 || vm.versions[0] != role || len(vm.probes) != 1 {
		t.Fatalf("versions %v probes %v", vm.versions, vm.probes)
	}
	pr := vm.probes[0]
	real, _ := filepath.EvalSymlinks(agent.SignInDirOf(l.h.Dir))
	if pr[0] != role || pr[1] != "node" || pr[2] != "-e" || pr[3] != vmProbeScript || pr[4] != real || len(pr) != 5 {
		t.Errorf("probe command: %q", pr[:3])
	}
	// other steps of the status are kept, and nothing is left running
	if st := l.t3.Status(); st["job"] != nil {
		t.Errorf("job: %v", st["job"])
	}
}

func TestT3SelfTestNeedsTheSignInButIsNotAFailure(t *testing.T) {
	l, _, role := selfLife(t)
	step := runSelf(t, l, map[string]any{})
	row := rowOf(t, step, role)
	if step["state"] != "needs_user" || row["state"] != "needs_user" || !strings.Contains(row["reason"].(string), "Sign in to Claude") {
		t.Errorf("step %v row %v", step["state"], row)
	}
	if !strings.Contains(lines(row), "no way out of its own") || strings.Contains(lines(row), "FAILED") {
		t.Errorf("the other checks should have run and passed:\n%s", lines(row))
	}
}

func TestT3SelfTestFailsOnEachBrokenCheck(t *testing.T) {
	for name, c := range map[string]struct {
		mod  func(l *life, vm *vmFake, role string)
		want string
	}{
		"direct IPv4 connects": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct1 ERR ENETUNREACH", "direct1 CONNECTED", 1)
		}, "may have a network of its own"},
		"second IPv4 connects": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct2 ERR EHOSTUNREACH", "direct2 CONNECTED", 1)
		}, "may have a network of its own"},
		"IPv6 connects": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct6 ERR EADDRNOTAVAIL", "direct6 CONNECTED", 1)
		}, "may have a network of its own"},
		"name resolves": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "dns ERR EAI_AGAIN", "dns RESOLVED", 1)
		}, "may have a network of its own"},
		"connection refused": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct1 ERR ENETUNREACH", "direct1 ERR ECONNREFUSED", 1)
		}, "may have a network of its own"},
		"connection reset": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct2 ERR EHOSTUNREACH", "direct2 ERR ECONNRESET", 1)
		}, "may have a network of its own"},
		"only a timeout": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct6 ERR EADDRNOTAVAIL", "direct6 ERR timeout", 1)
		}, "may have a network of its own"},
		"no direct answer": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "direct1 ERR ENETUNREACH\n", "", 1)
		}, "may have a network of its own"},
		"proxy refuses the API": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "proxy 200", "proxy 403", 1)
		}, "proxy didn't open api.anthropic.com"},
		"proxy error": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "proxy 200", "proxy ERR ECONNREFUSED", 1)
		}, "proxy didn't open api.anthropic.com"},
		"proxy opens the updater host": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "proxyhost 403", "proxyhost 200", 1)
		}, "did not refuse what it must refuse (proxyhost"},
		"proxy opens another port": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "proxyport 403", "proxyport 200", 1)
		}, "did not refuse what it must refuse (proxyport"},
		"proxy refusal missing": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "proxyhost 403\n", "", 1)
		}, "did not refuse what it must refuse"},
		"hook allows": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "hook deny", "hook allow", 1)
		}, "hook gate didn't answer"},
		"hook missing": {func(l *life, vm *vmFake, _ string) { vm.probeOut = strings.Replace(goodProbe, "hook deny\n", "", 1) }, "hook gate didn't answer"},
		"no deny rules": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "read=2 edit=2 bash=1", "read=0 edit=2 bash=1", 1)
		}, "not configured as expected"},
		"hooks not only": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "hooksOnly=true", "hooksOnly=false", 1)
		}, "not configured as expected"},
		"settings gone": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "settings hooksOnly=true read=2 edit=2 bash=1", "settings ERR ENOENT", 1)
		}, "not configured as expected"},
		"sign-in check unanswered": {func(l *life, vm *vmFake, _ string) {
			vm.probeOut = strings.Replace(goodProbe, "cred DENIED EACCES\n", "", 1)
		}, "check of the sign-in file gave no answer"},
		"version fails": {func(l *life, vm *vmFake, _ string) { vm.verErr = errors.New("image build failed") }, "image build failed"},
		"probe fails":   {func(l *life, vm *vmFake, _ string) { vm.probeErr = errors.New("this role already runs 1 session(s)") }, "already runs 1 session"},
		"instance gone": {func(l *life, vm *vmFake, role string) {
			p := filepath.Join(l.h.Fresh().T3BaseDir, "userdata", "settings.json")
			if err := os.WriteFile(p, []byte(`{"providerInstances":{}}`), 0o600); err != nil {
				panic(err)
			}
		}, "is not in T3's settings"},
		"shim stale": {func(l *life, vm *vmFake, role string) {
			if err := os.WriteFile(filepath.Join(l.h.Dir, "t3", "bin", "claude-"+role), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				panic(err)
			}
		}, "missing or out of date"},
		"T3 stopped": {func(l *life, vm *vmFake, role string) { l.t3.Stop() }, "start step"},
	} {
		t.Run(name, func(t *testing.T) {
			l, vm, role := selfLife(t)
			signIn(t, l)
			c.mod(l, vm, role)
			step := runSelf(t, l, map[string]any{})
			row := rowOf(t, step, role)
			if step["state"] != "failed" || row["state"] != "failed" || !strings.Contains(row["reason"].(string), c.want) {
				t.Fatalf("step %v row %v (want %q)", step["state"], row, c.want)
			}
			if p := lines(row); !strings.Contains(p, "FAILED: ") {
				t.Errorf("no failure line:\n%s", p)
			}
		})
	}
}

// A test stops at the first failure: later checks, and the VM, are not asked for.
func TestT3SelfTestStopsAtTheFirstFailure(t *testing.T) {
	l, vm, _ := selfLife(t)
	vm.verErr = errors.New("no image")
	runSelf(t, l, map[string]any{})
	if len(vm.probes) != 0 {
		t.Errorf("a VM was started after the version check failed: %v", vm.probes)
	}
}

func TestT3SelfTestSelectsRolesAndProjects(t *testing.T) {
	l, vm, first := selfLife(t)
	second := putRole(t, l.h, "builder")
	signIn(t, l)
	runSelf(t, l, map[string]any{"role": second})
	if len(vm.versions) != 1 || vm.versions[0] != second {
		t.Errorf("tested %v, wanted only %s", vm.versions, second)
	}
	vm.versions = nil
	step := runSelf(t, l, map[string]any{"project": "my-project"})
	if len(vm.versions) != 2 || rowOf(t, step, first)["state"] != "done" || rowOf(t, step, second)["state"] != "done" || step["state"] != "done" {
		t.Errorf("project run: %v %v", vm.versions, step)
	}
	for _, args := range []map[string]any{{"role": "r_unknown1"}, {"project": "elsewhere"}} {
		args["step"] = "selftest"
		if _, err := handle(t, l.h, "tok", "t3-setup.run", args); err == nil || !strings.Contains(err.Error(), "no such role or project") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestT3SelfTestShowsWorkingAndWaitsItsTurn(t *testing.T) {
	l, vm, role := selfLife(t)
	vm.gate = make(chan struct{})
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "selftest"}); err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	waitUntil(t, func() bool {
		st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
		step = stepByID(t, st, "selftest")
		return step["state"] == "working"
	})
	if rowOf(t, step, role)["state"] != "working" {
		t.Errorf("row: %v", rowOf(t, step, role))
	}
	if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "selftest"}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("a second self-test: %v", err)
	}
	close(vm.gate)
	l.t3.Wait()
}

func TestT3SelfTestRowBeforeAnyRunAndWithoutT3(t *testing.T) {
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
	step := stepByID(t, st, "selftest")
	row := rowOf(t, step, role)
	if step["state"] != "todo" || !strings.Contains(row["reason"].(string), "Needs T3 running and paired") {
		t.Errorf("before T3: %v", row)
	}
	l.up(t)
	st, _ = handle(t, l.h, "tok", "t3-setup.status", nil)
	if row = rowOf(t, stepByID(t, st, "selftest"), role); row["state"] != "todo" || row["reason"] != "Not run yet." || row["action"] != "selftest" {
		t.Errorf("before a run: %v", row)
	}
}

// Whether the agent can open the shared sign-in file is reported as it is: readable is a note and a reason on the row, not
// a pass dressed up as protection (decision 4: one folder for every role), and it does not fail the test.
func TestT3SelfTestSaysWhenTheAgentCanReadTheSignInFile(t *testing.T) {
	l, vm, role := selfLife(t)
	signIn(t, l)
	vm.probeOut = strings.Replace(goodProbe, "cred DENIED EACCES", "cred READABLE", 1)
	step := runSelf(t, l, map[string]any{})
	row := rowOf(t, step, role)
	if row["state"] != "done" || !strings.Contains(row["reason"].(string), "the agent can read the shared sign-in file") || !strings.Contains(row["reason"].(string), "decision 4") {
		t.Fatalf("row: %v", row)
	}
	if p := lines(row); !strings.Contains(p, "note: the agent can read the shared sign-in file: residual risk of decision 4") || strings.Contains(p, "can't open the shared") {
		t.Errorf("progress:\n%s", p)
	}
	// no file yet: not checked, and said so
	vm.probeOut = strings.Replace(goodProbe, "cred DENIED EACCES", "cred ABSENT", 1)
	row = rowOf(t, runSelf(t, l, map[string]any{}), role)
	if !strings.Contains(lines(row), "whether the agent could read it was not checked") || strings.Contains(row["reason"].(string), "Residual") {
		t.Errorf("absent: %v", row)
	}
}

// The probe script is built from the constants it must agree with: the hook gate's address is not typed twice.
func TestT3SelfTestProbeUsesTheHookGateAddress(t *testing.T) {
	if strings.Contains(vmProbeScript, "{{") {
		t.Error("a placeholder is left in the script")
	}
	host, port, _ := strings.Cut(agent.PipeHookAddr, ":")
	if !strings.Contains(vmProbeScript, `host: "`+host+`", port: `+port+`,`) {
		t.Errorf("the script doesn't name the hook gate %s", agent.PipeHookAddr)
	}
	if strings.Contains(vmProbeTemplate, "3130") || strings.Contains(vmProbeTemplate, "127.0.0.1") {
		t.Error("the hook gate's address is typed into the template")
	}
	// the sign-in file is opened for one byte and nothing of it is kept or printed
	if strings.Contains(vmProbeScript, `readFileSync(path.join(signin`) || !strings.Contains(vmProbeScript, "readSync(fd, Buffer.alloc(1), 0, 1, 0)") {
		t.Error("the script reads more of the sign-in file than one byte")
	}
}
