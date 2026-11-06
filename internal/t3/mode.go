// Package t3 is Rubi Home's bridge to a T3 Code server it runs itself: the sign-in to T3's MCP endpoint,
// a typed client for the thread tools, and the start and stop of the pinned server.
package t3

// Mode is a T3 runtime mode.
type Mode string

// The modes, from the most restricted to the least.
const (
	ApprovalRequired Mode = "approval-required"
	AutoAcceptEdits  Mode = "auto-accept-edits"
	Auto             Mode = "auto"
	FullAccess       Mode = "full-access"
)

var modeOrder = []Mode{ApprovalRequired, AutoAcceptEdits, Auto, FullAccess}

// norm turns an unknown mode into the most restricted one.
func (m Mode) norm() Mode {
	for _, k := range modeOrder {
		if m == k {
			return m
		}
	}
	return ApprovalRequired
}

func (m Mode) known() bool {
	for _, k := range modeOrder {
		if m == k {
			return true
		}
	}
	return false
}

func (m Mode) rank() int {
	for i, k := range modeOrder {
		if m == k {
			return i
		}
	}
	return 0
}

// Clamp returns the lower of want and ceiling. An unknown mode counts as approval-required.
func Clamp(want, ceiling Mode) Mode {
	if want.rank() < ceiling.rank() {
		return want.norm()
	}
	return ceiling.norm()
}

// PairingScopes gives the scopes a pairing code needs for an MCP access level: read-only reads, full-access
// also changes settings and source control, anything else reads and operates.
func PairingScopes(access string) []string {
	switch access {
	case "read-only":
		return []string{"orchestration:read"}
	case "full-access":
		return []string{"orchestration:read", "orchestration:operate", "settings:write", "source-control:write"}
	}
	return []string{"orchestration:read", "orchestration:operate"}
}
