//go:build unix

package home

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Rubi Home never opens the credential file claude keeps in the sign-in folder. The test puts a FIFO where the file is:
// opening a FIFO for reading blocks until someone writes to it, so any open() by the status, the sign-in or the
// self-test would hang and be caught here. (The file is not a regular file, so Home rightly says "no credential file".)
// The source-level counterpart is internal/repocheck.
func TestHomeNeverOpensTheCredentialFile(t *testing.T) {
	l, box, _, role := loginLife(t)
	dir := agent.SignInDirOf(l.h.Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, agent.CredentialFile), 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	vm := &vmFake{probeOut: goodProbe}
	vm.install(l.t3)
	l.up(t)

	done := make(chan struct{})
	var failures []string
	go func() {
		defer close(done)
		// status, twice
		for range 2 {
			st, err := handle(t, l.h, "tok", "t3-setup.status", nil)
			if err != nil {
				failures = append(failures, "status: "+err.Error())
				return
			}
			if s := stepByID(t, st, "login"); s["state"] != "todo" {
				failures = append(failures, "login state "+s["state"].(string))
			}
		}
		// the sign-in: the helper never writes the file here, so it ends without one
		box.script = func(b *loginBox, c *loginConn) {
			c.say("URL " + loginLink)
			c.say("CODE?")
			<-c.lines
			c.say("SAVED")
			c.finish()
		}
		if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "login", "method": "claudeai"}); err != nil {
			failures = append(failures, "login: "+err.Error())
			return
		}
		waitUntil(t, func() bool { s, _ := loginBlock(t, l); return s["wants_code"] == true })
		if _, err := handle(t, l.h, "tok", "t3-setup.login.code", map[string]any{"code": "CODE-1234"}); err != nil {
			failures = append(failures, "code: "+err.Error())
			return
		}
		waitUntil(t, func() bool { s, _ := loginBlock(t, l); return s["state"] == "failed" })
		// the self-test
		if _, err := handle(t, l.h, "tok", "t3-setup.run", map[string]any{"step": "selftest", "role": role}); err != nil {
			failures = append(failures, "selftest: "+err.Error())
			return
		}
		l.t3.Wait()
		st, _ := handle(t, l.h, "tok", "t3-setup.status", nil)
		row := rowOf(t, stepByID(t, st, "selftest"), role)
		if row["state"] != "needs_user" || !strings.Contains(lines(row), "no credential file yet") && !strings.Contains(row["reason"].(string), "Sign in") {
			failures = append(failures, "selftest row "+row["state"].(string))
		}
		// and the probe asked the VM, not Home, whether the agent could open it
		if len(vm.probes) != 1 {
			failures = append(failures, "probes")
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Home blocked: something opened the credential file (a FIFO there waits for a writer)")
	}
	for _, f := range failures {
		t.Error(f)
	}
	_ = context.Background
}
