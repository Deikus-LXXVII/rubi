package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentbridge"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// Explicit disposable installs only: never discover/use the live T3 installation
// or any account. No real provider/API credential is needed for server readiness.
func TestRealProviderBridgeSmoke(t *testing.T) {
	for _, name := range []string{"opencode", "opencode2", "t3"} {
		t.Run(name, func(t *testing.T) {
			binary := os.Getenv("RUBI_REAL_" + strings.ToUpper(name))
			if binary == "" {
				t.Skip("disposable real binary not supplied")
			}
			l, b, relay := bridgeFixture(t)
			l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, filepath.Dir(binary))
			// Public npm OpenCode launcher resolves its adjacent installed package.
			if name != "t3" {
				l.Policy.RuntimeRead = append(l.Policy.RuntimeRead, filepath.Dir(filepath.Dir(binary)))
			}
			home := filepath.Join(l.Policy.Login, "home")
			os.Mkdir(home, 0700)
			var err error
			b.Environment, err = agentbridge.StateEnvironment(l.Policy.Login)
			if err != nil {
				t.Fatal(err)
			}
			b.Command = []string{binary, "serve", "--hostname=127.0.0.1", "--port=37173"}
			if name == "t3" {
				base := filepath.Join(l.Policy.Login, "t3")
				os.Mkdir(base, 0700)
				b.Command = []string{binary, "serve", "--host", "127.0.0.1", "--port", "37173", "--base-dir", base, "--no-browser"}
				// Use the same native pairing scope list as production.
				b.PairCommand = []string{binary, "auth", "pairing", "create", "--base-dir", base, "--ttl", "10m", "--label", "disposable smoke", "--json"}
				for _, sc := range t3.PairingScopes("full-access") {
					b.PairCommand = append(b.PairCommand, "--scope", sc)
				}
			} else {
				l.Policy.AllowedDomains = append(l.Policy.AllowedDomains, "models.dev", "models.opencode.ai", "registry.npmjs.org", "opencode.ai")
				b.Environment["OPENCODE_CONFIG_CONTENT"] = "{}"
				b.Environment["OPENCODE_DISABLE_MODELS_FETCH"] = "1"
				b.Environment["OPENCODE_PASSWORD"] = strings.Repeat("synthetic-only", 4)
				b.Environment["OPENCODE_SERVER_PASSWORD"] = b.Environment["OPENCODE_PASSWORD"]
			}
			if name != "t3" {
				b.CheckPath = "/global/health"
				if name == "opencode2" {
					b.CheckPath = "/api/info"
				}
				b.CheckUser = "opencode"
				b.CheckPassword = b.Environment["OPENCODE_PASSWORD"]
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			process, ready, err := agentbridge.Start(ctx, l, relay, b)
			if err != nil {
				t.Fatal(err)
			}
			defer process.Close()
			hc := &http.Client{Transport: process, Timeout: 30 * time.Second}
			const base = "http://127.0.0.1:37173"
			if name == "t3" {
				tok, err := t3.Pair(ctx, hc, base, ready.Credential, "full-access", "disposable smoke")
				if err != nil {
					t.Fatal("native OAuth pairing over pipes failed (details suppressed)")
				}
				cl := t3.NewWithTransport(base, tok, t3.FullAccess, process)
				defer cl.Close()
				tools, err := cl.Tools(ctx)
				if err != nil || len(tools) == 0 {
					t.Fatal("native MCP readiness failed (details suppressed)")
				}
			} else {
				r, _ := http.NewRequestWithContext(ctx, "GET", base+b.CheckPath, nil)
				r.SetBasicAuth("opencode", b.Environment["OPENCODE_PASSWORD"])
				resp, err := hc.Do(r)
				if err != nil {
					t.Fatal("native health failed")
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("native health status: %d", resp.StatusCode)
				}
			}
		})
	}
}
