// Package icloudmail is the iCloud Mail integration. M1 ships only its manifest (for the catalog);
// the IMAP/SMTP implementation is ported from the prototype in M4.
package icloudmail

import (
	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/integrations"
)

const ID = "icloud-mail"

type mail struct{}

func init() { integrations.Register(mail{}) }

func (mail) Manifest() integrations.Manifest {
	return integrations.Manifest{
		ID:          ID,
		Name:        "iCloud Mail",
		Version:     "0.1.0",
		Description: "Read, search and draft iCloud email; send after your approval; get notified about replies.",
		Needs: "Your iCloud email address and an app-specific password created at account.apple.com " +
			"(Sign-In and Security > App-Specific Passwords). Apple offers no other way for apps to access iCloud Mail.",
		Secrets: []integrations.Secret{{
			Key:     "app_password",
			Label:   "App-specific password",
			Help:    "Create one named \"Rubi\" and paste it here. You can revoke it at any time.",
			HelpURL: "https://account.apple.com/account/manage",
		}},
		Actions: []integrations.Action{
			{Kind: ID + ".read", Title: "Read and search mail", DefaultLevel: approvals.None},
			{Kind: ID + ".draft", Title: "Save drafts", DefaultLevel: approvals.None},
			{Kind: ID + ".send", Title: "Send email", DefaultLevel: approvals.Strong, Options: []approvals.Option{
				{Key: "send", Label: "Send"},
				{Key: "send_track", Label: "Send and notify on reply", Meaning: "send, then watch for replies and notify you"},
			}},
		},
		Events: []integrations.EventType{{Type: "reply", Untrusted: []string{"reply.from", "reply.subject"}}},
		Egress: []string{"imap.mail.me.com:993", "smtp.mail.me.com:587"},
	}
}
