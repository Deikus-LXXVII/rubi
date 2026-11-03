package t3

import (
	"reflect"
	"testing"
)

func TestClamp(t *testing.T) {
	for _, c := range []struct{ want, ceiling, out Mode }{
		{FullAccess, ApprovalRequired, ApprovalRequired},
		{FullAccess, AutoAcceptEdits, AutoAcceptEdits},
		{FullAccess, Auto, Auto},
		{FullAccess, FullAccess, FullAccess},
		{Auto, FullAccess, Auto},
		{ApprovalRequired, FullAccess, ApprovalRequired},
		{AutoAcceptEdits, Auto, AutoAcceptEdits},
		{"bogus", FullAccess, ApprovalRequired},
		{FullAccess, "bogus", ApprovalRequired},
		{"", Auto, ApprovalRequired},
		{"bogus", "worse", ApprovalRequired},
	} {
		if got := Clamp(c.want, c.ceiling); got != c.out {
			t.Errorf("Clamp(%q, %q) = %q, want %q", c.want, c.ceiling, got, c.out)
		}
	}
}

func TestPairingScopes(t *testing.T) {
	read := []string{"orchestration:read"}
	operate := []string{"orchestration:read", "orchestration:operate"}
	full := []string{"orchestration:read", "orchestration:operate", "settings:write", "source-control:write"}
	for access, want := range map[string][]string{
		"read-only": read, "full-access": full, "approval-required": operate, "auto": operate, "": operate,
	} {
		if got := PairingScopes(access); !reflect.DeepEqual(got, want) {
			t.Errorf("PairingScopes(%q) = %v, want %v", access, got, want)
		}
	}
}
