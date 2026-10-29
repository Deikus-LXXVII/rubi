package mcpserver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentConnectKeyFile: a Bot hands its webhook key over as a file. Rubi (the daemon that serves the
// tool) reads it, deletes it, normalizes a pasted header, and the key never comes back. What a host's
// redaction leaves in a key argument is refused before any approval exists.
func TestAgentConnectKeyFile(t *testing.T) {
	r := newRig(t)
	srv, ch := hookServer(t)
	dir := t.TempDir() // under os.TempDir(), which is allowed
	path := filepath.Join(dir, ".rubi-hook-key")
	write := func(content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	gone := func() {
		t.Helper()
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("key file left behind (%v)", err)
		}
	}
	refused := func(args map[string]any, want string) {
		t.Helper()
		out := r.ag.call("rubi_agent_connect", args)
		if e := toString(out["tool_error"]); !strings.Contains(e, want) || strings.Contains(e, botKey) {
			t.Fatalf("want an error with %q: %v", want, out)
		}
	}
	base := func(extra map[string]any) map[string]any {
		m := map[string]any{"name": "Mail", "url": srv.URL, "reason": "to hear about mail"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	// What the host's $file: substitution produced, and a cut key: refused, with the key_file hint.
	refused(base(map[string]any{"key": "[REDACTED]"}), "key_file")
	refused(base(map[string]any{"key": "[REDACTED]\n"}), "redaction placeholder")
	refused(base(map[string]any{"key": "crsr_short"}), "only 10 characters")
	refused(base(map[string]any{"key": "$file:" + path}), "key_file")

	// Refused files are deleted too.
	write(botKey+"\n", 0o644)
	refused(base(map[string]any{"key_file": path}), "umask 077")
	gone()
	write("[REDACTED]\n", 0o600)
	refused(base(map[string]any{"key_file": path}), "redaction placeholder")
	gone()
	write(botKey, 0o600)
	refused(base(map[string]any{"key_file": path, "key": botKey}), "not both")
	gone()
	target := filepath.Join(dir, "target")
	write(botKey, 0o600)
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	refused(base(map[string]any{"key_file": path}), "symlink")
	gone()
	if _, err := os.Stat(target); err != nil {
		t.Fatal("the link's target was deleted")
	}
	refused(base(map[string]any{"key_file": "/etc/rubi-key-file-test"}), "must be inside")
	if pending := r.c.Approvals.Pending(); len(pending) != 0 {
		t.Fatalf("%d approvals for refused keys", len(pending))
	}

	// The happy path: the whole header, as printenv writes it.
	write("Authorization: Bearer "+botKey+"\n", 0o600)
	out := r.ag.call("rubi_agent_connect", base(map[string]any{"key_file": path}))
	gone()
	if b, _ := json.Marshal(out); strings.Contains(string(b), botKey) || out["status"] != "awaiting_approval" || out["user_pastes_webhook"] == true {
		t.Fatalf("propose with key_file: %v", out)
	}
	res, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(out["approval_id"].(string), "apply", pw)
	if err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	if h := waitHit(t, ch, "rubi.webhook.test"); h.auth != "Bearer "+botKey {
		t.Fatal("the test event doesn't carry the key from the file, normalized")
	}
	if !hasAgent(r, "Mail") {
		t.Fatal("not connected")
	}
}
