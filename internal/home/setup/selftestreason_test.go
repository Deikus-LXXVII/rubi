package setup

import "testing"

func TestSelfTestFailedReason(t *testing.T) {
	if got := SelfTestFailedReason(1, 1, "The role VM did not start."); got != "The self-test failed: the role VM did not start." {
		t.Fatalf("one role: %q", got)
	}
	if got := SelfTestFailedReason(1, 3, "The role VM did not start."); got != "1 of 3 roles failed." {
		t.Fatalf("one of several: %q", got)
	}
	if got := SelfTestFailedReason(2, 3, ""); got != "2 of 3 roles failed." {
		t.Fatalf("several: %q", got)
	}
}
