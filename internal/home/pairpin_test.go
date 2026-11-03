package home

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

func pairArgs(secret string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"secret": secret, "token": strings.Repeat("t", 40), "label": "Rubi x"})
	return b
}

func TestPairAcceptsThePINTypedLooselyOrTheFullSecret(t *testing.T) {
	for name, pick := range map[string]func(c homeproto.Code, pin string) string{
		"pin":    func(_ homeproto.Code, pin string) string { return strings.ToLower(homeproto.FormatPIN(pin)) },
		"secret": func(c homeproto.Code, _ string) string { return c.Secret },
	} {
		h := openHelper(t)
		code, pin, err := h.PairingCode(false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.pair(pairArgs(pick(code, pin))); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(h.Fresh().Paired) != 1 || pending(h) != nil {
			t.Fatalf("%s: paired %d, pending %v", name, len(h.Fresh().Paired), pending(h))
		}
	}
}

func TestPairVoidsTheCodeAfterFiveWrongSecrets(t *testing.T) {
	h := openHelper(t)
	_, pin, err := h.PairingCode(false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxPairAttempts-1; i++ {
		if _, err := h.pair(pairArgs("AAAA-AAAA")); err == nil {
			t.Fatal("a wrong PIN paired")
		}
		if p := pending(h); p == nil || p.Attempts != i+1 {
			t.Fatalf("after %d wrong tries: %+v", i+1, p)
		}
	}
	if _, err := h.pair(pairArgs("AAAA-AAAA")); err == nil {
		t.Fatal("a wrong PIN paired")
	}
	if pending(h) != nil {
		t.Fatal("the code survived five wrong tries")
	}
	// the right PIN is too late: the window has to be opened again
	if _, err := h.pair(pairArgs(pin)); err == nil {
		t.Fatal("a voided code paired")
	}
	if len(h.Fresh().Paired) != 0 {
		t.Fatal("paired")
	}
}

func TestPairFourWrongTriesStillLeaveTheRightPINWorking(t *testing.T) {
	h := openHelper(t)
	_, pin, _ := h.PairingCode(false)
	for i := 0; i < maxPairAttempts-1; i++ {
		_, _ = h.pair(pairArgs("BBBB-BBBB"))
	}
	if _, err := h.pair(pairArgs(pin)); err != nil {
		t.Fatal(err)
	}
}
