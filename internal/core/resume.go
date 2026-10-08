package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

const maxPlan = 2000

// ErrNoWebhook: setup isn't finished until Rubi can wake the agent.
var ErrNoWebhook = errors.New("finish setting up Rubi first: connect the agent webhook (see rubi_status, field " +
	"webhook.how). Without it Rubi can't tell you when the user approves something or a reply arrives")

// seenGrace is how long Rubi waits, after an approval is decided, for the agent to pick up the outcome
// itself (it is usually long-polling rubi_approval) before waking it through the webhook.
var seenGrace = 3 * time.Second

// MarkSeen records that the agent got an approval's final outcome directly, so no wake is needed.
func (c *Core) MarkSeen(approvalID string) {
	c.mu.Lock()
	c.seen[approvalID] = true
	c.mu.Unlock()
}

func (c *Core) takeSeen(approvalID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.seen[approvalID]
	delete(c.seen, approvalID)
	return s
}

type pendingPlan struct {
	plan, agent string
}

// SetPlan stores what the agent intends to do once a pending approval is decided. Rubi sends it back with
// the approval.decided event, so the agent can continue even if it is woken in a fresh run. agent names
// the Bot to wake ("" = the default agent).
func (c *Core) SetPlan(approvalID, plan, agent string) error {
	if agent != "" && !c.HasAgent(agent) {
		return fmt.Errorf("no agent %q is registered (agents: %s). Pass your Bot's name as registered, or register "+
			"yourself first: send the user rubi_link(\"agent:<your Bot name>\")", agent, c.agentNames())
	}
	plan = strings.TrimSpace(plan)
	if plan == "" {
		return errors.New("describe what you'll do after the decision")
	}
	if len(plan) > maxPlan {
		return errors.New("keep the plan under 2000 characters")
	}
	d, err := c.Approvals.Detail(approvalID)
	if err != nil {
		return err
	}
	if d.State != approvals.Pending {
		return errors.New("this approval is already " + string(d.State) + "; continue now")
	}
	c.mu.Lock()
	c.plans[approvalID] = pendingPlan{plan: plan, agent: agent}
	c.mu.Unlock()
	return nil
}

func (c *Core) takePlan(approvalID string) pendingPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.plans[approvalID]
	delete(c.plans, approvalID)
	return p
}

// WebhookConfigured reports whether Rubi can wake the agent.
func (c *Core) WebhookConfigured() bool {
	on := false
	_ = c.Vault.View(func(d *vault.Data) error {
		on = len(d.Agents) > 0
		return nil
	})
	return on
}

// AfterApproval tells the agent how it will learn about a decision, depending on whether Rubi can wake it.
func (c *Core) AfterApproval() string {
	if c.WebhookConfigured() {
		return "After sending the link, call rubi_continue_after(approval_id, plan, agent=<your Bot name>) with what you'll do once the " +
			"user decides. Then wait with rubi_approval(approval_id, wait_seconds=25). If it is still pending, end " +
			"your turn and tell the user you'll continue on your own: Rubi wakes you through the webhook with the " +
			"decision and your plan. (Without a plan you're still told the outcome, but may wake without context.)"
	}
	return "Rubi can't wake you when the user decides: no agent webhook is set up. Wait with " +
		"rubi_approval(approval_id, wait_seconds=25) while the user acts; if your turn has to end, ask the user to " +
		"tell you when they're done. Then suggest setting up the webhook so this happens automatically (see " +
		"rubi_status, field webhook)."
}

// WakeUp tells the agent how it learns about things later (an approval decided, a reply arriving), and
// forbids the expensive workaround agents reach for when they can't be woken.
func (c *Core) WakeUp() string {
	const noPolling = " Never create scheduled routines to check Rubi, mail or replies: every run costs the user's " +
		"quota, while Rubi already watches by itself at no cost."
	if c.WebhookConfigured() {
		return "Rubi wakes you through the agent webhook when something happens (an approval is decided, a reply " +
			"arrives)." + noPolling
	}
	return "No agent webhook is set up, so Rubi can't wake you; you'll see events (decisions, replies) in " +
		"rubi_events the next time the user writes." + noPolling + " Instead, offer to set up the webhook once " +
		"(see rubi_status, field webhook.how)."
}

const agentHow = "Create a routine named \"Rubi events\" with a webhook trigger (no schedule) whose instruction " +
	"is: \"A Rubi event arrived. Follow next_step in the JSON body.\" Then send the user " +
	"rubi_link(\"agent:<your Bot name>\") and tell them to open it in the Grok Bot desktop app, next to the routine: " +
	"its Webhook section shows the URL and key (only on desktop), which they paste into the page."

// WebhookHint describes the registered agents, or how to register.
func (c *Core) WebhookHint() map[string]any {
	if c.WebhookConfigured() {
		return map[string]any{"configured": true, "agents": c.Agents(),
			"note": "Several Bots can share Rubi. Identify yourself with your Bot's name where tools take an agent, " +
				"and pick your notifications with rubi_notifications. A Bot that isn't listed can connect itself: " + agentHow}
	}
	return map[string]any{"configured": false,
		"why": "Without it you only learn about approvals and events (like replies) when the user writes to you. " +
			"Don't work around it with scheduled routines: they cost the user's quota on every run.",
		"how": agentHow}
}
