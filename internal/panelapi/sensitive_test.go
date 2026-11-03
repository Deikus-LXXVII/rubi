package panelapi

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The pairing code of a Home computer is a secret: device.pair is sealed with forward secrecy, and the
// panel's list (panel/js/api.js SENSITIVE) names exactly the ops the server demands it for.
func TestSensitiveOpsMatchThePanelAndIncludeDevicePair(t *testing.T) {
	if !Sensitive("device.pair") {
		t.Fatal("device.pair carries the pairing secret and must be sealed with forward secrecy")
	}
	b, err := os.ReadFile("../../panel/js/api.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const SENSITIVE = new Set\(\[([^\]]*)\]\)`).FindSubmatch(b)
	if m == nil {
		t.Fatal("SENSITIVE not found in panel/js/api.js")
	}
	js := map[string]bool{}
	for _, f := range strings.Split(string(m[1]), ",") {
		if f = strings.Trim(strings.TrimSpace(f), `"`); f != "" {
			js[f] = true
		}
	}
	for op := range sensitiveOps {
		if !js[op] {
			t.Errorf("the panel doesn't seal %q with forward secrecy", op)
		}
	}
	for op := range js {
		if !sensitiveOps[op] {
			t.Errorf("the panel seals %q but the server doesn't demand it", op)
		}
	}
}

// An older panel sends device.pair unsealed (it didn't know the op was sensitive). The new Rubi refuses it
// before anything else happens, in words that tell the user what to do.
func TestUnsealedDevicePairFromAnOlderPanelSaysToReload(t *testing.T) {
	_, err := (&Server{}).dispatch(context.Background(), Envelope{Op: "device.pair", Args: []byte(`{"code":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "out of date") || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("%v", err)
	}
}
