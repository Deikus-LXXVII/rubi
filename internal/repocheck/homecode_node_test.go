package repocheck

// Runs the pure helpers of panel/js/homecode.js under node (skipped when node is not installed): the parse, the
// expiry, storage that throws, and the fingerprint, which must equal the one Rubi Home's window shows.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home"
)

func TestHomeCodeHelpersUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	h, err := home.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *home.Config) { c.Name = "Küche Mac" }); err != nil { // not ASCII: UTF-8 must survive
		t.Fatal(err)
	}
	code, _, err := h.PairingCode(false)
	if err != nil {
		t.Fatal(err)
	}
	link, full := code.WithoutSecret().String(), code.String()

	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(root(t), "panel/js/homecode.js"))
	if err != nil {
		t.Fatal(err)
	}
	mod := filepath.Join(dir, "homecode.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
import * as m from ` + fileURLJS(mod) + `;
const [link, full] = JSON.parse(process.argv.at(-1));
const out = {};
const mem = new Map();
globalThis.localStorage = { getItem: (k) => (mem.has(k) ? mem.get(k) : null), setItem: (k, v) => mem.set(k, String(v)), removeItem: (k) => mem.delete(k) };
const d = m.decodeHomeCode(link), f = m.decodeHomeCode(full);
out.link = d && { name: d.name, hasSecret: d.hasSecret, fp: await m.homeFingerprint(d.bundle) };
out.full = f && { hasSecret: f.hasSecret };
out.bad = ["", "junk", "rubi-home:", "rubi-home:!!!!!!!!!!!!!!!!!!!!!!", "rubi-home:" + btoa("{}").replace(/=+$/, "").padEnd(24, "A")].map((c) => m.decodeHomeCode(c));
out.kept = m.keepHomeCode(link, 1000);
out.until = JSON.parse(mem.get(m.HOME_CODE_KEY)).until - 1000;
out.early = m.waitingHomeCode(1000 + 14 * 60 * 1000) === link;
out.late = m.waitingHomeCode(1000 + 15 * 60 * 1000 + 1);
out.goneAfterExpiry = !mem.has(m.HOME_CODE_KEY);
out.keptFull = m.keepHomeCode(full, 1000);
out.fullStored = mem.has(m.HOME_CODE_KEY);
out.keptJunk = m.keepHomeCode("junk", 1000);
mem.set(m.HOME_CODE_KEY, "{not json");
out.damaged = m.waitingHomeCode(1);
out.goneAfterDamage = !mem.has(m.HOME_CODE_KEY);
m.keepHomeCode(link, 1000); m.clearHomeCode();
out.cleared = m.waitingHomeCode(1001);
Object.defineProperty(globalThis, "localStorage", { configurable: true, get() { throw new Error("denied"); } });
out.keptNoStorage = m.keepHomeCode(link, 1000);
out.waitNoStorage = m.waitingHomeCode(1000);
m.clearHomeCode();
Object.defineProperty(globalThis, "localStorage", { configurable: true, value: { getItem() { return null; }, setItem() { throw new Error("quota"); }, removeItem() { throw new Error("x"); } } });
out.keptFullStorage = m.keepHomeCode(link, 1000);
m.clearHomeCode();
out.vector = await m.homeFingerprint("abc");
console.log(JSON.stringify(out));
`
	arg, _ := json.Marshal([]string{link, full})
	cmd := exec.Command(node, "--input-type=module", "-e", script, "--", string(arg))
	cmd.Dir = dir
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got struct {
		Link *struct {
			Name      string
			HasSecret bool
			FP        string
		}
		Full            *struct{ HasSecret bool }
		Bad             []any
		Kept            bool
		Until           int64
		Early           bool
		Late            string
		GoneAfterExpiry bool
		KeptFull        bool
		FullStored      bool
		KeptJunk        bool
		Damaged         string
		GoneAfterDamage bool
		Cleared         string
		KeptNoStorage   bool
		WaitNoStorage   string
		KeptFullStorage bool
		Vector          string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if got.Link == nil || got.Link.Name != "Küche Mac" || got.Link.HasSecret {
		t.Errorf("the link's code: %+v", got.Link)
	}
	if got.Link != nil && got.Link.FP != h.Fingerprint() {
		t.Errorf("the panel's fingerprint %q differs from Rubi Home's %q", got.Link.FP, h.Fingerprint())
	}
	if got.Full == nil || !got.Full.HasSecret {
		t.Errorf("the full code must say it has a secret: %+v", got.Full)
	}
	for i, b := range got.Bad {
		if b != nil {
			t.Errorf("bad code %d was accepted: %v", i, b)
		}
	}
	if !got.Kept || got.Until != int64(15*time.Minute/time.Millisecond) {
		t.Errorf("kept %v, until +%d ms", got.Kept, got.Until)
	}
	if !got.Early || got.Late != "" || !got.GoneAfterExpiry {
		t.Errorf("expiry: early %v, late %q, deleted %v", got.Early, got.Late, got.GoneAfterExpiry)
	}
	if got.KeptFull || got.FullStored || got.KeptJunk {
		t.Errorf("stored what it must not: full %v/%v, junk %v", got.KeptFull, got.FullStored, got.KeptJunk)
	}
	if got.Damaged != "" || !got.GoneAfterDamage || got.Cleared != "" {
		t.Errorf("damaged %q deleted %v, cleared %q", got.Damaged, got.GoneAfterDamage, got.Cleared)
	}
	if got.KeptNoStorage || got.WaitNoStorage != "" || got.KeptFullStorage {
		t.Errorf("throwing storage: %v %q %v", got.KeptNoStorage, got.WaitNoStorage, got.KeptFullStorage)
	}
	// a fixed vector: sha256("abc") starts ba7816bf8f01, which is "ungWv48B" in base64url
	if got.Vector != "ungWv48B" {
		t.Errorf("vector %q", got.Vector)
	}
}

func fileURLJS(s string) string {
	b, _ := json.Marshal("file://" + s)
	return string(b)
}

// The panel and the approval hash the same text the same way: the whole bundle string, 6 bytes, base64url.
func TestFingerprintFormulaIsTheSameInGoAndTheApproval(t *testing.T) {
	devices := readRepo(t, "internal/core/devices.go")
	homeGo := readRepo(t, "internal/home/openwindow.go")
	js := readRepo(t, "panel/js/homecode.js")
	for name, src := range map[string]string{"core": devices, "home": homeGo} {
		if !strings.Contains(src, "sha256.Sum256([]byte(") || !strings.Contains(src, "RawURLEncoding.EncodeToString(sum[:6])") {
			t.Errorf("%s no longer hashes the bundle text to 6 bytes of base64url", name)
		}
	}
	if !strings.Contains(devices, "sha256.Sum256([]byte(pc.Bundle))") || !strings.Contains(homeGo, "sha256.Sum256([]byte(h.ID.PublicBundle()))") {
		t.Error("the bundle text is not what is hashed")
	}
	if !strings.Contains(js, `"SHA-256", new TextEncoder().encode(bundle)`) || !strings.Contains(js, "d.slice(0, 6)") {
		t.Error("the panel hashes something else")
	}
}
