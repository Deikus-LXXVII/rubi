package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Project rule 3 (docs/project-rules.md): known secret values go through the paths that talk to Bots, the panel and
// the audit log, and are searched for in everything Rubi writes: the log, the audit log (raw and opened with the
// vault key), the error texts and results returned to the Bot, and every file under Rubi's home. The routine's key
// may be on disk only inside the vault, which is sealed.
func TestSecretsNeverReachLogsOrTheAuditLog(t *testing.T) {
	var logs lockedBuf
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	c, hook, _ := botCore(t)
	dek, _ := c.Vault.Key()
	c.Audit.SetVaultKey(dek) // the audit log seals its fields only once Rubi was unlocked or paired
	ctx := context.Background()

	var returned []string
	note := func(v any, err error) {
		b, _ := json.Marshal(v)
		returned = append(returned, string(b), fmt.Sprint(err), fmt.Sprintf("%+v", v))
	}

	// a Bot proposes its routine's key; the key is refused once (not a routine address), then taken, then approved
	_, err := c.ProposeBot(ctx, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: "https://evil.example/x", Key: testHookKey}})
	note(nil, err)
	out, err := c.ProposeBot(ctx, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: "Authorization: Bearer " + testHookKey}})
	note(out, err)
	id := out["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	note(det, nil)
	// a wrong proof, then the right one
	_, err = c.Approvals.DecidePanel(ctx, id, "no-such-option", true, approvals.Verified)
	note(nil, err)
	snap, err := c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	note(snap, err)

	// a second Bot, whose key the user pastes on the approval screen: the typed value must not be logged either
	const pasted = "crsr_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	out2, err := c.ProposeBot(ctx, BotRequest{Agent: "Other", Webhook: &WebhookProposal{URL: hook}})
	note(out2, err)
	if err == nil {
		id2 := out2["approval_id"].(string)
		snap2, err := c.Approvals.DecidePanelWith(ctx, id2, "apply", true, approvals.Verified,
			map[string]string{inputWebhookURL: hook, inputWebhookKey: "Bearer " + pasted})
		note(snap2, err)
		d2, _ := c.Approvals.Detail(id2)
		note(d2, nil)
	}

	// links and the library code: a ticket in a link and a Bot's library code are secrets too
	link, err := c.Link("unlock") // the link goes to the Bot on purpose; it must not reach the log or the audit
	if err != nil {
		t.Fatal(err)
	}
	ticket := ""
	if i := strings.Index(link, "&t="); i > 0 {
		ticket = link[i+3:]
		if j := strings.IndexAny(ticket, "&"); j > 0 {
			ticket = ticket[:j]
		}
	}
	botCode := c.agentCodeFor("Mail")

	secrets := map[string]string{"routine key": testHookKey, "pasted routine key": pasted, "link ticket": ticket, "bot code": botCode}
	for name, s := range secrets {
		if len(s) < 12 {
			t.Fatalf("the test's %s is too short to be searched for: %q", name, s)
		}
	}

	c.Lock() // the log and the audit are complete once nothing else is pending
	waitGone(t, c, "Mail")

	rawAudit, _ := os.ReadFile(c.Layout.Audit())
	haystacks := map[string]string{"log": logs.String(), "audit file": string(rawAudit), "returned": strings.Join(returned, "\n")}
	for name, hay := range haystacks {
		for what, s := range secrets {
			if name == "returned" && what == "link ticket" { // the Bot gets its link
				continue
			}
			if strings.Contains(hay, s) {
				t.Errorf("the %s is in the %s: %.200q", what, name, hay)
			}
		}
	}
	if len(rawAudit) == 0 {
		t.Fatal("the flow wrote no audit record: the test would prove nothing")
	}
	// the files under Rubi's home hold no secret in the clear (the vault is sealed)
	_ = filepath.Walk(c.Layout.Home, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for what, s := range secrets {
			if what != "link ticket" && strings.Contains(string(b), s) { // a ticket lives in memory only
				t.Errorf("the %s is on disk in the clear: %s", what, p)
			}
		}
		return nil
	})
}

// The audit log, opened with the vault key, holds what happened and not what was typed.
func TestOpenedAuditHoldsNoSecret(t *testing.T) {
	c, hook, _ := botCore(t)
	dek, _ := c.Vault.Key()
	c.Audit.SetVaultKey(dek)
	ctx := context.Background()
	out, err := c.ProposeBot(ctx, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Approvals.DecidePanel(ctx, out["approval_id"].(string), "apply", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, c)
	if len(events) == 0 {
		t.Fatal("no audit events: the test would prove nothing")
	}
	b, _ := json.Marshal(events)
	for _, s := range []string{testHookKey, strings.TrimPrefix(testHookKey, "crsr_"), c.agentCodeFor("Mail")} {
		if strings.Contains(string(b), s) {
			t.Fatalf("the opened audit log holds a secret: %s", b)
		}
	}
	waitGone(t, c, "Mail")
}
