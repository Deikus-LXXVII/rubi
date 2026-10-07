// Package icloudmail is the iCloud Mail integration: read, search and draft freely (by default), send only
// after the user's approval, and get notified when someone replies to a tracked email.
//
// Apple offers no OAuth for iCloud Mail; third-party apps use an app-specific password over IMAP/SMTP.
// The user enters it in the Rubi panel, never in the agent chat.
package icloudmail

import (
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/integrations"
)

const ID = "icloud-mail"

const (
	kindRead  = ID + ".read"
	kindDraft = ID + ".draft"
	kindSend  = ID + ".send"
)

type integration struct {
	mu      sync.Mutex
	host    integrations.Host
	stop    chan struct{}
	pollNow chan struct{}
}

func init() { integrations.Register(&integration{}) }

func (*integration) Manifest() integrations.Manifest {
	return integrations.Manifest{
		ID:          ID,
		Name:        "iCloud Mail",
		Version:     "0.1.0",
		Description: "Read, search and draft iCloud email; send after your approval; get notified about replies.",
		Needs: "Your iCloud email address and an app-specific password created at account.apple.com " +
			"(Sign-In and Security > App-Specific Passwords). Apple offers no other way for apps to access iCloud Mail.",
		Fields: []integrations.Field{
			{Key: "address", Label: "iCloud email address", Type: "email", Placeholder: "name@icloud.com", Required: true,
				Help: "Use your @icloud.com (or @me.com) address, even if your Apple ID uses another email."},
			{Key: "from_name", Label: "Your name (shown to recipients)", Type: "text", Placeholder: "Optional"},
		},
		Secrets: []integrations.Secret{{
			Key:     "app_password",
			Label:   "App-specific password",
			Help:    "On account.apple.com open Sign-In and Security, then App-Specific Passwords, create one named \"Rubi\", and paste it here. You can revoke it there at any time.",
			HelpURL: "https://account.apple.com/account/manage",
		}},
		Actions: []integrations.Action{
			{Kind: kindRead, Title: "Read and search mail", DefaultLevel: approvals.None},
			{Kind: kindDraft, Title: "Save drafts", DefaultLevel: approvals.None},
			{Kind: kindSend, Title: "Send email", DefaultLevel: approvals.Strong, Options: sendOptions},
		},
		Events: []integrations.EventType{{Type: "reply", Untrusted: []string{"reply.from", "reply.subject"}}},
		Egress: []string{"imap.mail.me.com:993", "smtp.mail.me.com:587"},
	}
}

var sendOptions = []approvals.Option{
	{Key: "send", Label: "Send"},
	{Key: "send_track", Label: "Send and notify on reply", Meaning: "send, then watch for replies and notify you"},
}
