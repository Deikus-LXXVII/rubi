package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// listing makes a role's runner list the given session ids.
func listing(ids ...string) func(string, *FakeRunner) {
	return func(_ string, fr *FakeRunner) {
		fr.Handle = func(method string, _ json.RawMessage) (any, error, bool) {
			if method != "session.list" {
				return nil, nil, false
			}
			rows := []any{}
			for _, id := range ids {
				rows = append(rows, map[string]any{"id": id})
			}
			return map[string]any{"sessions": rows}, nil, true
		}
	}
}

func TestRoleSessionOpsRefuseForeignSessions(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 6))
	re.setup = listing("mine-1")
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x") // boots the VM
	eventually(t, "slot", func() bool { return re.running() == 1 })
	calls := func(method string) int { return len(re.vm(a).Runner().CallsOf(method)) }

	for _, op := range []string{"send", "messages", "rename", "fork", "interrupt", "close", "mode"} {
		args := map[string]any{"project": "my-project", "role": a, "id": "foreign-1", "text": "hi", "name": "n", "mode": "plan"}
		if _, err := re.op(op, args); err == nil || !strings.Contains(err.Error(), "doesn't belong to role") {
			t.Errorf("%s of a foreign session: %v", op, err)
		}
	}
	for _, method := range []string{"session.send", "session.messages", "session.rename", "session.fork", "session.interrupt", "session.close", "session.mode"} {
		if n := calls(method); n != 0 {
			t.Errorf("%s reached the runner %d times", method, n)
		}
	}
	// The role's own session (listed by its runner) goes through.
	if _, err := re.op("send", map[string]any{"project": "my-project", "role": a, "id": "mine-1", "text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if n := calls("session.send"); n != 1 {
		t.Fatalf("send reached the runner %d times", n)
	}
	if _, err := re.op("messages", map[string]any{"project": "my-project", "role": a, "id": "mine-1"}); err != nil {
		t.Fatal(err)
	}
	// A session that holds a slot is the role's own whatever the listing says.
	if _, err := re.op("rename", map[string]any{"project": "my-project", "role": a, "id": "sess-1", "name": "n"}); err != nil {
		t.Fatal(err)
	}
}

func TestRoleReadsDoNotBootOrKeepTheVMUp(t *testing.T) {
	re := newRoleEnv(t, setLimits(1, 4), func(o *Options) { o.RoleVMIdle = func() time.Duration { return 15 * time.Minute } })
	a, _ := re.put("role-a", nil)
	// A stopped VM is not started by a read.
	for _, op := range []string{"sessions", "messages"} {
		res, err := re.op(op, map[string]any{"project": "my-project", "role": a, "id": "x-1"})
		if err != nil || res["vm_state"] != "stopped" {
			t.Fatalf("%s: %v %v", op, res, err)
		}
	}
	if re.vm(a) != nil && re.vm(a).Starts() != 0 {
		t.Fatal("a read started the VM")
	}
	for _, op := range []string{"interrupt", "mode"} {
		if _, err := re.op(op, map[string]any{"project": "my-project", "role": a, "id": "x-1", "mode": "plan"}); err == nil {
			t.Errorf("%s on a stopped VM went through", op)
		}
	}
	// A read of a running VM doesn't reset its idle clock.
	re.startRole(a, "x")
	re.vm(a).Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "slot freed", func() bool { return re.running() == 0 })
	re.advance(14 * time.Minute)
	if _, err := re.op("sessions", map[string]any{"project": "my-project", "role": a}); err != nil {
		t.Fatal(err)
	}
	re.advance(2 * time.Minute)
	re.m.sweepIdle()
	if re.vm(a).State() != StateStopped {
		t.Fatalf("a read kept the VM up: %s", re.vm(a).State())
	}
}
