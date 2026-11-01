package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// decide submits a strong approval for plugin/kind, approves it as the user would, and runs fn while the
// approval is being executed (as pluginSubmit does for a real plugin).
func decide(t *testing.T, c *Core, plugin, kind string, payload json.RawMessage, fn func(id string)) {
	t.Helper()
	var id string
	out, err := c.Approvals.Submit(context.Background(), approvals.Request{Integration: plugin, Kind: kind, Summary: "s",
		Options: []approvals.Option{{Key: "approve", Label: "Approve"}},
		Execute: func(ctx context.Context, option string) (any, error) {
			defer c.beginExec(plugin, kind, payload)()
			fn(id)
			return map[string]any{}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	id = out["approval_id"].(string)
	if _, err := c.Approvals.DecidePanel(context.Background(), id, "approve", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
}

// attest runs attestHomeCall and drops the forwarded arguments.
func attest(c *Core, plugin, op string, args json.RawMessage) error {
	_, err := c.attestHomeCall(plugin, op, args)
	return err
}

func putArgs(mcpApproval, changeApproval string) json.RawMessage {
	return json.RawMessage(`{"id":"r","base_version":1,"by":"bot-a","spec":{"mcp":[{"name":"x","type":"http","url":"https://example.com/mcp","approval":"` +
		mcpApproval + `"}]},"change":{"approval":"` + changeApproval + `"}}`)
}

// A real, user-given approval of the right kind is accepted; a made-up id, another plugin's approval, an
// approval of another kind, a pending one and a denied one are refused.
func TestHomeGateRolePutApprovals(t *testing.T) {
	c := targetCore(t)
	decide(t, c, "claude-code", "claude-code.role_widen", nil, func(id string) {
		if err := attest(c, "claude-code", opRolePut, putArgs(id, id)); err != nil {
			t.Errorf("a real approval was refused: %v", err)
		}
		if err := attest(c, "claude-code", opRolePut, putArgs("", "")); err != nil {
			t.Errorf("a put without approvals was refused: %v", err)
		}
		if err := attest(c, "claude-code", opRolePut, putArgs("apr_made_up", id)); err == nil {
			t.Error("a fabricated id was accepted")
		}
		if err := attest(c, "claude-code", opRolePut, putArgs(id, "apr_made_up")); err == nil {
			t.Error("a fabricated change id was accepted")
		}
		if err := attest(c, "other-plugin", opRolePut, putArgs(id, id)); err == nil {
			t.Error("another plugin used this plugin's approval")
		}
	})
	decide(t, c, "claude-code", "claude-code.fork", nil, func(id string) {
		if err := attest(c, "claude-code", opRolePut, putArgs(id, "")); err == nil {
			t.Error("an approval of another kind was accepted")
		}
	})
	out, err := c.Approvals.Submit(context.Background(), approvals.Request{Integration: "claude-code", Kind: "claude-code.role_widen", Summary: "s",
		Options: []approvals.Option{{Key: "approve", Label: "Approve"}}, Execute: func(context.Context, string) (any, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	id := out["approval_id"].(string)
	if err := attest(c, "claude-code", opRolePut, putArgs(id, "")); err == nil {
		t.Error("a pending approval was accepted")
	}
	_, _ = c.Approvals.DecidePanel(context.Background(), id, "", false, approvals.Verified)
	if err := attest(c, "claude-code", opRolePut, putArgs(id, "")); err == nil {
		t.Error("a denied approval was accepted")
	}
}

const sshArgs = `{"name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc","ask_each_time":true}`

// ssh.register goes through only while an approval of kind ssh_target with the same target is being executed.
func TestHomeGateSSHRegister(t *testing.T) {
	c := targetCore(t)
	if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("registered with no approval")
	}
	payload := json.RawMessage(`{"device":"d","name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc","ask_each_time":true}`)
	decide(t, c, "claude-code", kindSSHTarget, payload, func(string) {
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(sshArgs)); err != nil {
			t.Errorf("the approved target was refused: %v", err)
		}
		other := strings.Replace(sshArgs, "example.com", "evil.example.net", 1)
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(other)); err == nil {
			t.Error("a different host than the one approved was accepted")
		}
		if err := attest(c, "other-plugin", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
			t.Error("another plugin used the grant")
		}
	})
	decide(t, c, "claude-code", "claude-code.role_widen", payload, func(string) {
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
			t.Error("an approval of another kind allowed a registration")
		}
	})
	if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("the grant outlived the approval")
	}
}

// Every field Home's ssh.register takes is bound to the approval: widening one that the user did not see is refused.
func TestHomeGateSSHRegisterAllFields(t *testing.T) {
	c := targetCore(t)
	const base = `"name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc"`
	payload := json.RawMessage(`{"device":"d","key_type":"ssh-ed25519",` + base + `,"allowed_commands":["ls","uptime"],"ask_each_time":true}`)
	decide(t, c, "claude-code", kindSSHTarget, payload, func(string) {
		try := func(extra string) error {
			return attest(c, "claude-code", opSSHRegister, json.RawMessage(`{`+base+extra+`}`))
		}
		if err := try(`,"allowed_commands":["uptime","ls","ls"],"ask_each_time":true`); err != nil {
			t.Errorf("the approved target (commands in another order) was refused: %v", err)
		}
		for name, extra := range map[string]string{
			"no allowed list":   `,"ask_each_time":true`,
			"one more command":  `,"allowed_commands":["ls","uptime","sh"],"ask_each_time":true`,
			"one fewer command": `,"allowed_commands":["ls"],"ask_each_time":true`,
			"ask_each_time off": `,"allowed_commands":["ls","uptime"]`,
			"reuse":             `,"allowed_commands":["ls","uptime"],"ask_each_time":true,"reuse":true`,
			"an unknown field":  `,"allowed_commands":["ls","uptime"],"ask_each_time":true,"sudo":true`,
		} {
			if try(extra) == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})
	// A payload without the optional fields allows only args without them (missing equals false).
	decide(t, c, "claude-code", kindSSHTarget, json.RawMessage(`{`+base+`}`), func(string) {
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(`{`+base+`,"ask_each_time":false,"reuse":false}`)); err != nil {
			t.Errorf("false equals missing: %v", err)
		}
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(`{`+base+`,"allowed_commands":["ls"]}`)); err == nil {
			t.Error("a command list the approval did not carry was accepted")
		}
		if err := attest(c, "claude-code", opSSHRegister, json.RawMessage(`{`+base+`,"reuse":true}`)); err == nil {
			t.Error("reuse the approval did not carry was accepted")
		}
	})
}

// A skill accept carries the id the plugin made; core vouches for it while that approval executes.
func TestHomeGateSkillAcceptID(t *testing.T) {
	c := targetCore(t)
	args := func(id string) json.RawMessage {
		return json.RawMessage(`{"spec":{},"change":{"approval":"` + id + `"}}`)
	}
	if err := attest(c, "claude-code", opRolePut, args("sk_1")); err == nil {
		t.Error("an unknown skill-accept id was accepted")
	}
	decide(t, c, "claude-code", "claude-code.role_skill_accept", json.RawMessage(`{"approval_id":"sk_1"}`), func(string) {
		if err := attest(c, "claude-code", opRolePut, args("sk_1")); err != nil {
			t.Errorf("the executing skill accept was refused: %v", err)
		}
		if err := attest(c, "claude-code", opRolePut, args("sk_2")); err == nil {
			t.Error("another id was accepted")
		}
		if err := attest(c, "other-plugin", opRolePut, args("sk_1")); err == nil {
			t.Error("another plugin used the id")
		}
	})
	if err := attest(c, "claude-code", opRolePut, args("sk_1")); err == nil {
		t.Error("the id outlived the approval")
	}
}

// The gate sits on the relay: homeCall refuses before it looks for a device.
func TestHomeCallRunsTheGate(t *testing.T) {
	c := targetCore(t)
	_, err := c.homeCall(context.Background(), "claude-code", []string{"claude-agent"}, "", opSSHRegister, json.RawMessage(sshArgs))
	if err == nil || !strings.Contains(err.Error(), "Rubi refused") {
		t.Fatalf("%v", err)
	}
}

// Core tells Home which kind each attested id is, and a plugin-sent attested_kinds never survives.
func TestHomeGateAttestedKinds(t *testing.T) {
	c := targetCore(t)
	forged := json.RawMessage(`{"id":"r","base_version":1,"attested_kinds":{"x":"claude-code.role_widen"},"spec":{},"change":{"approval":"` + "%s" + `"}}`)
	kindsOf := func(out json.RawMessage) map[string]string {
		var o struct {
			K map[string]string `json:"attested_kinds"`
		}
		if err := json.Unmarshal(out, &o); err != nil {
			t.Fatal(err)
		}
		return o.K
	}
	decide(t, c, "claude-code", "claude-code.role_skill_accept", nil, func(id string) {
		out, err := c.attestHomeCall("claude-code", opRolePut, json.RawMessage(strings.Replace(string(forged), "%s", id, 1)))
		if err != nil {
			t.Fatal(err)
		}
		k := kindsOf(out)
		if len(k) != 1 || k[id] != "claude-code.role_skill_accept" {
			t.Errorf("attested_kinds = %v", k)
		}
	})
	decide(t, c, "claude-code", "claude-code.role_widen", nil, func(id string) {
		out, err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, id))
		if err != nil {
			t.Fatal(err)
		}
		if k := kindsOf(out); len(k) != 1 || k[id] != "claude-code.role_widen" {
			t.Errorf("attested_kinds = %v", k)
		}
	})
	// The plugin-made id of a running skill accept is a skill-accept id.
	payload := json.RawMessage(`{"approval_id":"sk_made"}`)
	decide(t, c, "claude-code", kindSkillAccept, payload, func(string) {
		out, err := c.attestHomeCall("claude-code", opRolePut, putArgs("", "sk_made"))
		if err != nil {
			t.Fatal(err)
		}
		if k := kindsOf(out); k["sk_made"] != kindSkillAccept {
			t.Errorf("attested_kinds = %v", k)
		}
	})
	// No ids: the forged field is replaced by an empty map.
	out, err := c.attestHomeCall("claude-code", opRolePut, json.RawMessage(`{"attested_kinds":{"a":"claude-code.role_widen"},"spec":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if k := kindsOf(out); len(k) != 0 {
		t.Errorf("a forged attested_kinds survived: %v", k)
	}
}
