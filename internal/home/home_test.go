package home

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
)

// startHelper runs a helper on a test relay and returns it with a pairing code.
func startHelper(t *testing.T) (*Helper, homeproto.Code, *relaytest.Relay) {
	t.Helper()
	rl := relaytest.New()
	t.Cleanup(rl.Close)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) { c.Relays = []string{rl.URL()} }); err != nil {
		t.Fatal(err)
	}
	key, _ := h.RelayKey()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ready := make(chan int, 4)
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: h.Handle}
	go (&relay.Server{Key: key, Relays: h.Relays(), Handle: rpc.HandleRPC, Ready: func(n int) { ready <- n }}).Run(ctx)
	<-ready
	code, err := h.StartPairing()
	if err != nil {
		t.Fatal(err)
	}
	return h, code, rl
}

func call(t *testing.T, c *homeproto.Client, op string, args, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Call(ctx, op, args, out)
}

func TestPairing(t *testing.T) {
	h, code, _ := startHelper(t)
	parsed, err := homeproto.ParseCode(" \n" + code.String()[:20] + "\n" + code.String()[20:] + " ")
	if err != nil || parsed.Relay != code.Relay {
		t.Fatalf("parse: %v", err)
	}
	if _, err := homeproto.ParseCode("rubi-home:xyz"); err == nil {
		t.Fatal("damaged code accepted")
	}
	token := homeproto.RandomToken()
	c, err := homeproto.NewClient(code.Relay, code.Bundle, code.Relays, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := call(t, c, "hello", nil, nil); err == nil || !strings.Contains(err.Error(), "isn't paired") {
		t.Fatalf("before pairing: %v", err)
	}
	if err := call(t, c, "pair", map[string]string{"secret": "wrong", "token": token}, nil); err == nil {
		t.Fatal("wrong secret accepted")
	}
	var hello map[string]any
	if err := call(t, c, "pair", map[string]string{"secret": code.Secret, "token": token, "label": "my Rubi"}, &hello); err != nil {
		t.Fatal(err)
	}
	if hello["shortcuts_folder"] != "Rubi" {
		t.Fatalf("hello: %v", hello)
	}
	if err := call(t, c, "pair", map[string]string{"secret": code.Secret, "token": homeproto.RandomToken()}, nil); err == nil {
		t.Fatal("pairing code worked twice")
	}
	if err := call(t, c, "hello", nil, nil); err != nil {
		t.Fatalf("after pairing: %v", err)
	}
	// Only a hash of the token is on disk.
	raw, _ := os.ReadFile(filepath.Join(h.Dir, "config.json"))
	if strings.Contains(string(raw), token) {
		t.Fatal("token stored in the clear")
	}
	// Another client with a made-up token gets nothing.
	other, _ := homeproto.NewClient(code.Relay, code.Bundle, code.Relays, homeproto.RandomToken())
	defer other.Close()
	if err := call(t, other, "shortcuts.list", nil, nil); err == nil {
		t.Fatal("unpaired token accepted")
	}
	// A client that pinned another identity can't read the answers.
	wrong, _ := Open(t.TempDir())
	imp, _ := homeproto.NewClient(code.Relay, wrong.ID.PublicBundle(), code.Relays, token)
	defer imp.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := imp.Call(ctx, "hello", nil, nil); err == nil {
		t.Fatal("answer accepted from the wrong identity")
	}
}

func TestShortcutsFolderOnly(t *testing.T) {
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	h.Shortcuts = func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "list":
			if args[2] != "Rubi" {
				t.Errorf("listed folder %q", args[2])
			}
			return []byte("Heating on (11111111-2222-3333-4444-555555555555)\nGood night (AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE)\n"), nil
		case "run":
			ran = append(ran, args[1])
			for i, a := range args {
				if a == "--output-path" {
					_ = os.WriteFile(args[i+1], []byte("21.5 °C"), 0o600)
				}
			}
			return nil, nil
		}
		return nil, nil
	}
	list, err := h.listShortcuts(context.Background())
	if err != nil || len(list.(map[string]any)["shortcuts"].([]string)) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	out, err := h.runShortcut(context.Background(), "Heating on", "")
	if err != nil || out.(map[string]any)["output"] != "21.5 °C" || ran[0] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("run: %v %v %v", out, err, ran)
	}
	if _, err := h.runShortcut(context.Background(), "Delete everything", ""); err == nil {
		t.Fatal("ran a shortcut outside the folder")
	}
}

func TestHue(t *testing.T) {
	pressed := false
	var lastKey string
	bridge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api" && r.Method == http.MethodPost:
			if !pressed {
				_, _ = w.Write([]byte(`[{"error":{"type":101,"description":"link button not pressed"}}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"success":{"username":"appkey123","clientkey":"x"}}]`))
		case strings.HasPrefix(r.URL.Path, "/clip/v2/resource/light"):
			lastKey = r.Header.Get("hue-application-key")
			_, _ = w.Write([]byte(`{"errors":[],"data":[{"id":"l1"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer bridge.Close()
	config := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bridgeid":"001788FFFE123456"}`))
	}))
	defer config.Close()

	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Hue.Insecure = true
	// In the test the plain config endpoint and the TLS API live on two ports; point bridgeID at the
	// config server by wrapping Pair's address lookup.
	ip := strings.TrimPrefix(bridge.URL, "https://")
	h.Hue.configHost = strings.TrimPrefix(config.URL, "http://")
	h.Hue.find = func(context.Context) []string { return []string{ip} }

	ctx := context.Background()
	if _, err := h.Hue.Pair(ctx, ip); err == nil || !strings.Contains(err.Error(), "link button") {
		t.Fatalf("before the button: %v", err)
	}
	pressed = true
	res, err := h.Hue.Pair(ctx, ip)
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["key"] != "appkey123" || m["bridge"] != "001788fffe123456" || h.Config().HuePins["001788fffe123456"] == "" {
		t.Fatalf("pair: %v", m)
	}
	out, err := h.Hue.Do(ctx, HueRequest{Bridge: "001788FFFE123456", IP: ip, Key: "appkey123", Method: "get", Path: "/clip/v2/resource/light"})
	if err != nil || lastKey != "appkey123" {
		t.Fatalf("request: %v %v", out, err)
	}
	var body struct{ Data []map[string]string }
	_ = json.Unmarshal(out.(map[string]any)["body"].(json.RawMessage), &body)
	if len(body.Data) != 1 {
		t.Fatalf("body: %v", out)
	}
	if _, err := h.Hue.Do(ctx, HueRequest{Bridge: "001788fffe123456", IP: ip + "/api/appkey123/config?x=", Key: "appkey123",
		Method: "GET", Path: "/clip/v2/resource/light"}); err == nil {
		t.Error("a path smuggled in the address was accepted")
	}
	for _, bad := range []HueRequest{
		{Method: "DELETE", Path: "/clip/v2/resource/light"},
		{Method: "GET", Path: "/api/appkey123/config"},
		{Method: "GET", Path: "/clip/v2/resource/../../api"},
	} {
		bad.Bridge, bad.IP, bad.Key = "001788fffe123456", ip, "appkey123"
		if _, err := h.Hue.Do(ctx, bad); err == nil {
			t.Errorf("allowed %s %s", bad.Method, bad.Path)
		}
	}
	// A different device at the bridge's address is refused (its certificate doesn't match the pin).
	impostor := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	impostor.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	impostor.StartTLS()
	defer impostor.Close()
	_, err = h.Hue.Do(ctx, HueRequest{Bridge: "001788fffe123456", IP: strings.TrimPrefix(impostor.URL, "https://"),
		Key: "appkey123", Method: "GET", Path: "/clip/v2/resource/light"})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("impostor: %v", err)
	}
	// The bridge got a new address: it is found again by its id.
	gone := httptest.NewServer(nil)
	goneAddr := strings.TrimPrefix(gone.URL, "http://")
	gone.Close()
	out, err = h.Hue.Do(ctx, HueRequest{Bridge: "001788fffe123456", IP: goneAddr, Key: "appkey123", Method: "GET", Path: "/clip/v2/resource/light"})
	if err != nil || out.(map[string]any)["ip"] != ip {
		t.Fatalf("moved bridge: %v %v", out, err)
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "001788fffe123456"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestComputerNameHasNoOwner(t *testing.T) {
	for host, want := range map[string]string{
		"Annas-MacBook-Pro.local": "MacBook Pro", "Anna's MacBook Air": "MacBook Air", "Mac-mini": "Mac mini",
		"studio-server": "studio server", "Bob’s Desktop": "Desktop", "": "Home computer",
	} {
		if got := computerName(host); got != want {
			t.Errorf("%q: %q, want %q", host, got, want)
		}
	}
}

// A Rubi's beats reach the helper's watcher without a pairing token (a locked Rubi can't read its token),
// but only signed and with a registration sealed to this helper.
func TestWatchBeats(t *testing.T) {
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.Watch = watch.New(h.ID.Box)
	_, rubiKey, _ := ed25519.GenerateKey(rand.Reader)
	reg := watch.Registration{WatchKey: base64.RawURLEncoding.EncodeToString(rubiKey.Public().(ed25519.PublicKey)),
		Name: "r", Admin: "Main", URL: "https://example.com/hook", Key: "k"}
	sealed, err := watch.Seal(h.ID.Box.PublicKey(), reg)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(watch.NewMessage(rubiKey, "locked", "", sealed))
	if _, err := h.Handle(context.Background(), homeproto.Envelope{Op: "watch.beat", Args: args}); err != nil {
		t.Fatalf("beat refused: %v", err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bad, _ := json.Marshal(watch.NewMessage(other, "locked", "", sealed))
	if _, err := h.Handle(context.Background(), homeproto.Envelope{Op: "watch.beat", Args: bad}); err == nil {
		t.Fatal("a forged beat was accepted")
	}
	// Everything else still needs the pairing.
	if _, err := h.Handle(context.Background(), homeproto.Envelope{Op: "shortcuts.list"}); err == nil {
		t.Fatal("an unpaired call went through")
	}
}
