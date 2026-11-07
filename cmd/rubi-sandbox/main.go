// Command rubi-sandbox runs the disposable pieces of a beta-test sandbox: throwaway keys, a loopback
// relay, a fake Rubi Home, a fake Claude sign-in page, and a signed local plugin catalog.
// It is for the people who work on Rubi. Nothing it prints is a signing key, a pairing code, or a session token.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/identity"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/sandbox"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "lo":
		if err := sandbox.LoUp(); err != nil {
			fail(err)
		}
	case "keys":
		dir := needFlag(os.Args[2:], "--dir")
		k, err := sandbox.InitKeys(dir)
		if err != nil {
			fail(err)
		}
		fmt.Println(k.PluginPub)
	case "publish":
		args := os.Args[2:]
		keys, err := sandbox.InitKeys(needFlag(args, "--keys"))
		if err != nil {
			fail(err)
		}
		if err := sandbox.Publish(keys, needFlag(args, "--plugin"), needFlag(args, "--out"), needFlag(args, "--source")); err != nil {
			fail(err)
		}
	case "stack":
		if err := stack(os.Args[2:]); err != nil {
			fail(err)
		}
	case "mcp":
		if err := mcpCall(os.Args[2:]); err != nil {
			fail(err)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rubi-sandbox lo | keys --dir DIR | publish --keys DIR --plugin BIN --out DIR --source URL | stack --state DIR --run DIR --files DIR | mcp --socket PATH --tool NAME --args JSON --out FILE")
	os.Exit(2)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "rubi-sandbox:", err)
	os.Exit(1)
}

func needFlag(args []string, name string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == name && args[i+1] != "" {
			return args[i+1]
		}
	}
	fail(fmt.Errorf("missing %s", name))
	return ""
}

func stack(args []string) error {
	state := needFlag(args, "--state")
	run := needFlag(args, "--run")
	files := needFlag(args, "--files")
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(run, 0o700); err != nil {
		return err
	}
	rel, err := sandbox.ListenRelay("127.0.0.1:18791")
	if err != nil {
		return err
	}
	defer rel.Close()
	oauth, port, err := sandbox.ListenOAuth("127.0.0.1:18443", filepath.Join(state, "oauth.crt"), filepath.Join(state, "oauth.key"))
	if err != nil {
		return err
	}
	defer oauth.Close()
	fs := &http.Server{Addr: "127.0.0.1:18081", Handler: http.FileServer(http.Dir(files)), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", fs.Addr)
	if err != nil {
		return err
	}
	go fs.Serve(ln)
	defer fs.Close()
	hook, hookURL, err := sandbox.ListenHook("127.0.0.1:18082")
	if err != nil {
		return err
	}
	defer hook.Close()

	id, err := identity.LoadOrCreate(filepath.Join(state, "identity.json"))
	if err != nil {
		return err
	}
	key, err := loadRelayKey(filepath.Join(state, "relay.hex"))
	if err != nil {
		return err
	}
	secret, err := loadSecret(filepath.Join(state, "pair.secret"))
	if err != nil {
		return err
	}
	signIn := sandbox.SignInURL(port)
	home := sandbox.NewHome("Sandbox Home", secret, signIn)
	hs := &homeproto.Server{Key: id.Box, Handle: func(_ context.Context, env homeproto.Envelope) (any, error) {
		return home.Handle(env)
	}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go (&relay.Server{Key: key, Relays: []string{rel.URL}, Handle: hs.HandleRPC, Logf: func(string, ...any) {}}).Run(ctx)

	code := homeproto.Code{Relay: key.Public(), Bundle: id.PublicBundle(), Relays: []string{rel.URL}, Secret: secret, Name: "Sandbox Home"}
	if err := os.WriteFile(filepath.Join(run, "home-code"), []byte(code.String()+"\n"), 0o600); err != nil {
		return err
	}
	pub := map[string]any{
		"name": "Sandbox Home", "relay": rel.URL,
		"catalog":    "http://127.0.0.1:18081/marketplace/catalog.json",
		"signin_url": signIn, "signin_code": sandbox.SignInCode,
		"webhook_url": hookURL,
		"code_file":   "home-code",
	}
	b, _ := json.MarshalIndent(pub, "", "  ")
	if err := os.WriteFile(filepath.Join(run, "public.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(run, "stack.ready"), []byte("ok\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "rubi-sandbox: stack ready")
	<-ctx.Done()
	return nil
}

func loadRelayKey(path string) (*relay.Key, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		return relay.KeyFromHex(string(trimNL(b)))
	}
	k, err := relay.NewKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(k.Hex()+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func loadSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(trimNL(b)) > 0 {
		return string(trimNL(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(s+"\n"), 0o600); err != nil {
		return "", err
	}
	return s, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func mcpCall(args []string) error {
	socket := needFlag(args, "--socket")
	name := needFlag(args, "--tool")
	raw := needFlag(args, "--args")
	out := needFlag(args, "--out")
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "rubi-sandbox", Version: "dev"}, nil).Connect(ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	var arguments map[string]any
	if raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
			return err
		}
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return err
	}
	body, _ := json.MarshalIndent(map[string]any{"is_error": res.IsError, "structured": res.StructuredContent, "text": textOf(res)}, "", "  ")
	return os.WriteFile(out, append(body, '\n'), 0o600)
}

func textOf(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}
