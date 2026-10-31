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

func putArgs(mcpApproval, changeApproval string) json.RawMessage {
	return json.RawMessage(`{"id":"r","base_version":1,"by":"bot-a","spec":{"mcp":[{"name":"x","type":"http","url":"https://example.com/mcp","approval":"` +
		mcpApproval + `"}]},"change":{"approval":"` + changeApproval + `"}}`)
}

// A real, user-given approval of the right kind is accepted; a made-up id, another plugin's approval, an
// approval of another kind, a pending one and a denied one are refused.
func TestHomeGateRolePutApprovals(t *testing.T) {
	c := targetCore(t)
	decide(t, c, "claude-code", "claude-code.role_widen", nil, func(id string) {
		if err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, id)); err != nil {
			t.Errorf("a real approval was refused: %v", err)
		}
		if err := c.attestHomeCall("claude-code", opRolePut, putArgs("", "")); err != nil {
			t.Errorf("a put without approvals was refused: %v", err)
		}
		if err := c.attestHomeCall("claude-code", opRolePut, putArgs("apr_made_up", id)); err == nil {
			t.Error("a fabricated id was accepted")
		}
		if err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, "apr_made_up")); err == nil {
			t.Error("a fabricated change id was accepted")
		}
		if err := c.attestHomeCall("other-plugin", opRolePut, putArgs(id, id)); err == nil {
			t.Error("another plugin used this plugin's approval")
		}
	})
	decide(t, c, "claude-code", "claude-code.fork", nil, func(id string) {
		if err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, "")); err == nil {
			t.Error("an approval of another kind was accepted")
		}
	})
	out, err := c.Approvals.Submit(context.Background(), approvals.Request{Integration: "claude-code", Kind: "claude-code.role_widen", Summary: "s",
		Options: []approvals.Option{{Key: "approve", Label: "Approve"}}, Execute: func(context.Context, string) (any, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	id := out["approval_id"].(string)
	if err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, "")); err == nil {
		t.Error("a pending approval was accepted")
	}
	_, _ = c.Approvals.DecidePanel(context.Background(), id, "", false, approvals.Verified)
	if err := c.attestHomeCall("claude-code", opRolePut, putArgs(id, "")); err == nil {
		t.Error("a denied approval was accepted")
	}
}

const sshArgs = `{"name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc","ask_each_time":true}`

// ssh.register goes through only while an approval of kind ssh_target with the same target is being executed.
func TestHomeGateSSHRegister(t *testing.T) {
	c := targetCore(t)
	if err := c.attestHomeCall("claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("registered with no approval")
	}
	payload := json.RawMessage(`{"device":"d","name":"web","host":"example.com","port":22,"user":"deploy","fingerprint":"SHA256:abc","ask_each_time":true}`)
	decide(t, c, "claude-code", kindSSHTarget, payload, func(string) {
		if err := c.attestHomeCall("claude-code", opSSHRegister, json.RawMessage(sshArgs)); err != nil {
			t.Errorf("the approved target was refused: %v", err)
		}
		other := strings.Replace(sshArgs, "example.com", "evil.example.net", 1)
		if err := c.attestHomeCall("claude-code", opSSHRegister, json.RawMessage(other)); err == nil {
			t.Error("a different host than the one approved was accepted")
		}
		if err := c.attestHomeCall("other-plugin", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
			t.Error("another plugin used the grant")
		}
	})
	decide(t, c, "claude-code", "claude-code.role_widen", payload, func(string) {
		if err := c.attestHomeCall("claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
			t.Error("an approval of another kind allowed a registration")
		}
	})
	if err := c.attestHomeCall("claude-code", opSSHRegister, json.RawMessage(sshArgs)); err == nil {
		t.Fatal("the grant outlived the approval")
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
