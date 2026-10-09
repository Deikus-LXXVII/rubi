package panelapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/e2e"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
)

func setup(t *testing.T) (*core.Core, *httptest.Server) {
	t.Helper()
	c, err := core.Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(panelapi.New(c).Handler())
	t.Cleanup(srv.Close)
	c.SetEndpoint(srv.URL)
	return c, srv
}

func client(t *testing.T, c *core.Core, purpose string) *panelclient.Client {
	t.Helper()
	link, err := c.Link(purpose)
	if err != nil {
		t.Fatal(err)
	}
	l, err := panelclient.ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := panelclient.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func TestPairUnlockFlow(t *testing.T) {
	c, _ := setup(t)

	pc := client(t, c, "pair")
	var h panelclient.Hello
	if err := pc.Call("hello", nil, &h); err != nil || h.State != "unpaired" {
		t.Fatalf("hello: %+v %v", h, err)
	}

	bad := *pc
	bad.Link.Pairing = "wrong-code"
	if _, err := bad.PairWithPassword("correct horse"); err == nil {
		t.Fatal("paired with a wrong code")
	}
	v, err := pc.PairWithPassword("correct horse")
	if err != nil || c.State() != core.Unlocked {
		t.Fatalf("pair: %v state=%s", err, c.State())
	}
	if _, err := pc.PairWithPassword("again"); err == nil {
		t.Fatal("paired twice")
	}

	c.Lock()
	uc := client(t, c, "unlock")

	noTicket := *uc
	noTicket.Link.Ticket = ""
	if err := noTicket.Call("unlock.keys", nil, nil); err == nil {
		t.Fatal("wrapped keys served without a ticket")
	}
	if _, err := uc.UnlockWithPassword("wrong", v); err == nil || c.State() != core.Locked {
		t.Fatalf("wrong password unlocked: %v", err)
	}
	v2, err := uc.UnlockWithPassword("correct horse", v)
	if err != nil || c.State() != core.Unlocked || v2 <= v {
		t.Fatalf("unlock: %v state=%s v=%d->%d", err, c.State(), v, v2)
	}

	var st struct {
		State    string           `json:"state"`
		Receipts []map[string]any `json:"receipts"`
	}
	if err := uc.Call("status", nil, &st); err != nil || st.State != "unlocked" || len(st.Receipts) != 2 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if err := uc.Call("lock", nil, nil); err != nil || c.State() != core.Locked {
		t.Fatalf("lock: %v", err)
	}
}

func TestReplayAndStaleRequestsRejected(t *testing.T) {
	c, srv := setup(t)
	pub, _ := e2e.ParseInstanceBundle(c.ID.PublicBundle())
	ch := e2e.Client{InstanceKey: pub}

	send := func(env panelapi.Envelope) string {
		plain, _ := json.Marshal(env)
		req, respKey, _ := ch.Seal(panelapi.RPCPath, plain)
		body, _ := json.Marshal(req)
		post := func() string {
			resp, err := http.Post(srv.URL+panelapi.RPCPath, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var sealed e2e.Response
			_ = json.NewDecoder(resp.Body).Decode(&sealed)
			out, err := e2e.OpenResponse(respKey, env.RID, sealed)
			if err != nil {
				t.Fatal(err)
			}
			return string(out)
		}
		first := post()
		second := post() // the exact same bytes, as an eavesdropper would replay them
		return first + "\n" + second
	}

	got := send(panelapi.Envelope{Op: "hello", TS: nowUnix(), RID: "rid-1"})
	parts := strings.Split(got, "\n")
	if !strings.Contains(parts[0], `"ok":true`) || !strings.Contains(parts[1], "replayed") {
		t.Fatalf("replay: %s", got)
	}
	stale := send(panelapi.Envelope{Op: "hello", TS: nowUnix() - 3600, RID: "rid-2"})
	if !strings.Contains(strings.Split(stale, "\n")[0], "off by more than") {
		t.Fatalf("stale: %s", stale)
	}
}

func TestUndecryptableRequestRevealsNothing(t *testing.T) {
	_, srv := setup(t)
	resp, err := http.Post(srv.URL+panelapi.RPCPath, "application/json",
		strings.NewReader(`{"v":1,"epk":"AAAA","nonce":"AAAA","ct":"AAAA"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(b)) != "bad request" {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
}

func TestCORSOnlyForPanelOrigin(t *testing.T) {
	_, srv := setup(t)
	for origin, want := range map[string]string{"https://rubi-panel.com": "https://rubi-panel.com", "https://evil.example": ""} {
		req, _ := http.NewRequest(http.MethodOptions, srv.URL+panelapi.RPCPath, nil)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
			t.Fatalf("origin %s: allow-origin %q", origin, got)
		}
	}
}

func nowUnix() int64 { return time.Now().Unix() }
