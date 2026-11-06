package repocheck

// The Ed25519 test vectors of the Windows installer's own verifier (internal/repocheck/testdata/ed25519, README.md there):
// they are checked here against Go's crypto/ed25519, and the script that runs them through the installer's code is run when
// PowerShell is here.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type edVector struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	PK     string `json:"pk"`
	Msg    string `json:"msg"`
	Sig    string `json:"sig"`
	Valid  bool   `json:"valid"`
}

func edVectors(t *testing.T) []edVector {
	t.Helper()
	var f struct {
		Vectors []edVector `json:"vectors"`
	}
	if err := json.Unmarshal([]byte(readRepo(t, "internal/repocheck/testdata/ed25519/vectors.json")), &f); err != nil {
		t.Fatal(err)
	}
	return f.Vectors
}

func mustHex(t *testing.T, v edVector, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("%s %q: bad hex: %v", v.Source, v.Name, err)
	}
	return b
}

// leInt reads little-endian bytes as a number.
func leInt(b []byte) *big.Int {
	r := make([]byte, len(b))
	for i := range b {
		r[len(b)-1-i] = b[i]
	}
	return new(big.Int).SetBytes(r)
}

// The vector file is the one the installer's verifier is held to. Go's crypto/ed25519 agrees with it on every vector except two
// where Go accepts a non-canonical public key and the installer's strict check refuses it. Any other difference means the file
// drifted (or Go changed), so it fails and a person looks.
func TestEd25519VectorsAgreeWithGoExceptTheTwoKnown(t *testing.T) {
	known := map[string]bool{
		"derived|the neutral point as the key, encoded with y = p + 1": true,
		"speccheck|case 11": true,
	}
	seen := map[string]bool{}
	for _, v := range edVectors(t) {
		pk, msg, sig := mustHex(t, v, v.PK), mustHex(t, v, v.Msg), mustHex(t, v, v.Sig)
		got := len(pk) == ed25519.PublicKeySize && ed25519.Verify(pk, msg, sig) // Verify panics on a key of another length
		key := v.Source + "|" + v.Name
		if got != v.Valid {
			if !known[key] {
				t.Errorf("%s: Go says %v, the file says %v", key, got, v.Valid)
			}
			if !got || v.Valid {
				t.Errorf("%s: the known difference is that Go accepts and the strict check refuses; here Go %v, file %v", key, got, v.Valid)
			}
			seen[key] = true
		} else if known[key] {
			t.Errorf("%s: no longer differs from Go; update the README and this test", key)
		}
	}
	for k := range known {
		if !seen[k] {
			t.Errorf("the known difference %q was not seen", k)
		}
	}
}

// The file holds what it must: the five RFC 8032 vectors (valid), all 210 Wycheproof cases, the 12 speccheck cases, and at
// least one vector of each class a lax verifier gets wrong (S >= L, a non-canonical key, a non-canonical R, a negative-zero
// key), found from the bytes, not from the names.
func TestEd25519VectorsAreComplete(t *testing.T) {
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	l := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 252), mustBig("27742317777372353535851937790883648493"))
	count := map[string]int{}
	wp := map[string]bool{}
	var sGeL, nonCanonA, nonCanonR, negZeroA int
	for _, v := range edVectors(t) {
		count[v.Source]++
		if v.Source == "rfc8032" && !v.Valid {
			t.Errorf("RFC 8032 vector %q is not valid", v.Name)
		}
		if v.Source == "wycheproof" {
			m := regexp.MustCompile(`^tcId ([0-9]+) \(`).FindStringSubmatch(v.Name)
			if m == nil {
				t.Errorf("wycheproof vector %q has no tcId", v.Name)
			} else {
				wp[m[1]] = true
			}
		}
		pk, sig := mustHex(t, v, v.PK), mustHex(t, v, v.Sig)
		if len(pk) != 32 || len(sig) != 64 {
			continue
		}
		if leInt(sig[32:]).Cmp(l) >= 0 {
			sGeL++
		}
		yOf := func(b []byte) *big.Int {
			c := append([]byte(nil), b...)
			c[31] &= 0x7f
			return leInt(c)
		}
		if yOf(pk).Cmp(p) >= 0 {
			nonCanonA++
		}
		if yOf(sig[:32]).Cmp(p) >= 0 {
			nonCanonR++
		}
		// x = 0 exactly when y = 1 or y = p - 1; with the sign bit set that is "negative zero"
		if y := yOf(pk); pk[31]&0x80 != 0 && (y.Cmp(big.NewInt(1)) == 0 || y.Cmp(new(big.Int).Sub(p, big.NewInt(1))) == 0) {
			negZeroA++
		}
	}
	for src, want := range map[string]int{"rfc8032": 5, "wycheproof": 210, "speccheck": 12} {
		if count[src] != want {
			t.Errorf("%d %s vectors, want %d", count[src], src, want)
		}
	}
	for id := 1; id <= 210; id++ {
		if !wp[strconv.Itoa(id)] {
			t.Errorf("Wycheproof tcId %d is missing", id)
		}
	}
	for name, n := range map[string]int{"S >= L": sGeL, "a non-canonical A": nonCanonA, "a non-canonical R": nonCanonR, "a negative-zero A": negZeroA} {
		if n == 0 {
			t.Errorf("no vector with %s", name)
		}
	}
}

func mustBig(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic(s)
	}
	return n
}

// If PowerShell is here, the script that runs these vectors through the installer's own verifier (taken from install-home.ps1)
// passes. CI runs it under Windows PowerShell 5.1 and PowerShell 7.
func TestInstallerEd25519ScriptPassesWithPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", filepath.Join(root(t), "scripts", "test-install-home-ed25519.ps1"))
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ok") {
		t.Errorf("the script did not say ok:\n%s", out)
	}
}
