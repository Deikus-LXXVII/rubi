package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keyFileDir makes an allowed directory (the only one, for this test) and a sibling that isn't.
func keyFileDir(t *testing.T) (allowed, outside string) {
	t.Helper()
	base := t.TempDir()
	allowed, outside = filepath.Join(base, "allowed"), filepath.Join(base, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	oldDirs, oldTemp := keyFileDirs, keyFileTempDir
	keyFileDirs, keyFileTempDir = []string{allowed}, func() string { return allowed }
	t.Cleanup(func() { keyFileDirs, keyFileTempDir = oldDirs, oldTemp })
	return allowed, outside
}

func writeKey(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
}

func gone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s still exists (%v)", path, err)
	}
}

func TestReadKeyFile(t *testing.T) {
	allowed, outside := keyFileDir(t)
	p := filepath.Join(allowed, ".rubi-hook-key")
	writeKey(t, p, testHookKey+"\n", 0o600)
	if got, err := ReadKeyFile(p); err != nil || got != testHookKey {
		t.Fatalf("ReadKeyFile = %q, %v", got, err)
	}
	gone(t, p)

	writeKey(t, p, "  Authorization: Bearer "+testHookKey+"\r\n", 0o400)
	if got, err := ReadKeyFile(p); err != nil || got != "Authorization: Bearer "+testHookKey {
		t.Fatalf("0400 file: %q, %v (whitespace trimmed, the header is normalized later)", got, err)
	}
	gone(t, p)

	target := filepath.Join(allowed, "target")
	tests := []struct {
		name    string
		setup   func() string
		deleted bool
	}{
		{"group-readable", func() string { writeKey(t, p, testHookKey, 0o640); return p }, true},
		{"world-readable", func() string { writeKey(t, p, testHookKey, 0o644); return p }, true},
		{"too large", func() string { writeKey(t, p, strings.Repeat("a", maxKeyFile+1), 0o600); return p }, true},
		{"empty", func() string { writeKey(t, p, "\n", 0o600); return p }, true},
		{"symlink", func() string {
			writeKey(t, target, testHookKey, 0o600)
			_ = os.Remove(p)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			return p
		}, true},
		{"directory", func() string {
			d := filepath.Join(allowed, "dir")
			_ = os.Mkdir(d, 0o700)
			return d
		}, false},
		{"outside the allowed dirs", func() string {
			o := filepath.Join(outside, "key")
			writeKey(t, o, testHookKey, 0o600)
			return o
		}, false},
		{"through a linked directory", func() string {
			o := filepath.Join(outside, "key")
			writeKey(t, o, testHookKey, 0o600)
			l := filepath.Join(allowed, "link")
			_ = os.Remove(l)
			if err := os.Symlink(outside, l); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(l, "key")
		}, false},
		{"dot-dot", func() string { return allowed + "/../outside/key" }, false},
		{"relative", func() string { return "key" }, false},
		{"missing", func() string { return filepath.Join(allowed, "nope") }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup()
			got, err := ReadKeyFile(path)
			if err == nil {
				t.Fatalf("accepted: %q", got)
			}
			if strings.Contains(err.Error(), testHookKey) {
				t.Fatalf("the error repeats the key: %v", err)
			}
			if tt.deleted {
				gone(t, path)
			}
		})
	}
	// A link is removed, never what it points to.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
	// Nothing outside the allowed directory is ever deleted.
	if _, err := os.Stat(filepath.Join(outside, "key")); err != nil {
		t.Fatalf("a file outside the allowed dirs was deleted: %v", err)
	}
}

func TestBotWebhookKey(t *testing.T) {
	const routine = "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066"
	// A real routine key is crsr_ and 86 more characters (91 in all).
	real := "crsr_" + strings.Repeat("aB3_-", 17) + "x"
	if len(real) != 91 {
		t.Fatal(len(real))
	}
	tests := []struct {
		name, in, url, want, errHas string
	}{
		{"bare", real, routine, real, ""},
		{"bearer", "Bearer " + real, routine, real, ""},
		{"header", "Authorization: Bearer " + real, routine, real, ""},
		{"header, newline from printenv", "Authorization: Bearer " + real + "\n", routine, real, ""},
		{"other receiver", "s3cret-token-0123456789", "http://127.0.0.1:9/hook", "s3cret-token-0123456789", ""},
		{"redacted", "[REDACTED]", routine, "", "redaction placeholder"},
		{"redacted with newline", "[REDACTED]\n", routine, "", "redaction placeholder"},
		{"redacted after Bearer", "Bearer [REDACTED]", "http://127.0.0.1:9/hook", "", "redaction placeholder"},
		{"masked", "********************************", "http://127.0.0.1:9/hook", "", "redaction placeholder"},
		{"short", "crsr_abc123", routine, "", "only 11 characters"},
		{"short other receiver", "s3cret", "http://127.0.0.1:9/hook", "", "only 6 characters"},
		{"whitespace inside", "crsr_abcdefghij klmnopqrstuvwxyz", routine, "", "spaces"},
		{"not a routine key", "s3cret-token-0123456789", routine, "", "isn't a Grok Bot routine key"},
		{"routine key with odd characters", "crsr_abcdefghijklmnop.qrstuvwxyz", routine, "", "isn't a Grok Bot routine key"},
		{"empty header", "Authorization: Bearer", routine, "", "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := botWebhookKey(tt.in, tt.url)
			if tt.errHas == "" {
				if err != nil || got != tt.want {
					t.Fatalf("got %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errHas) || !strings.Contains(err.Error(), "key_file") {
				t.Fatalf("got %q, %v; want an error with %q and the key_file hint", got, err, tt.errHas)
			}
		})
	}
}

// TestProposeRefusesRedactedKey: what the host's substitution produced is refused before any approval.
func TestProposeRefusesRedactedKey(t *testing.T) {
	c, hook, _ := botCore(t)
	for _, k := range []string{"[REDACTED]", "[REDACTED]\n", "crsr_short"} {
		if out, err := c.ProposeBot(t.Context(), BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: k}}); err == nil {
			t.Fatalf("%q accepted: %v", k, out)
		}
	}
	if n := len(c.Approvals.Pending()); n != 0 {
		t.Fatalf("%d approvals created for refused keys", n)
	}
}

// TestCleanRoutineKey: a pasted key for a Grok Bot routine address gets the same checks as a Bot's key,
// and the error never repeats it; other receivers keep the plain cleaning.
func TestCleanRoutineKey(t *testing.T) {
	const routine = "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066"
	const other = "http://127.0.0.1:9/hook"
	real := "crsr_" + strings.Repeat("aB3_-", 17) + "x"
	tests := []struct {
		name, in, url, want, errHas string
	}{
		{"header", "Authorization: Bearer " + real, routine, real, ""},
		{"other receiver keeps short keys", "s3cret", other, "s3cret", ""},
		{"redacted", "[REDACTED]", routine, "", "redaction placeholder"},
		{"masked", "****************************", routine, "", "redaction placeholder"},
		{"short", "crsr_abc123", routine, "", "only 11 characters"},
		{"wrong prefix", "s3cret-token-0123456789", routine, "", "isn't a Grok Bot routine key"},
		{"odd characters", "crsr_abcdefghijklmnop.qrstuvwxyz", routine, "", "isn't a Grok Bot routine key"},
		{"missing", "Bearer", routine, "", "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanRoutineKey(tt.in, tt.url)
			if tt.errHas == "" {
				if err != nil || got != tt.want {
					t.Fatalf("got %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.errHas) {
				t.Fatalf("got %q, %v; want an error with %q", got, err, tt.errHas)
			}
			if k := strings.TrimSpace(tt.in); len(k) > 8 && strings.Contains(err.Error(), k) {
				t.Fatalf("the error repeats the key: %v", err)
			}
		})
	}
}
