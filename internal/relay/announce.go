package relay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Release announcements. When the site publishes a release or a catalog change, it also publishes a signed
// Nostr event (NIP-78 application data, kind 30078, d="rubi-releases") to the default relays. Relays keep the
// latest one, so a Rubi that was offline gets it when it connects, and connected ones get it at once.
//
// An announcement is only a hint to check now. Nothing in it is trusted: Rubi then reads the release feed
// and the catalog and verifies every signature as usual. So a forged announcement costs at most a check.

const (
	AnnounceKind = 30078
	AnnounceTag  = "rubi-releases"
)

// GatewayURL is Rubi Gateway, the relay run for Rubi only (see internal/gateway). Only its name is
// built in; nothing about the machine behind it.
const GatewayURL = "wss://gateway.rubi-panel.com"

// AnnouncePub is the public key the site signs announcements with (x-only secp256k1, hex).
const AnnouncePub = "bd5b70c28e0f8fe748503ffcbb3407112fb3b597d9275573f40ad53807b99eb8"

// AnnouncerKey returns the trusted announcement key; test builds may override it.
func AnnouncerKey(version string) string {
	if version == "dev" || strings.HasSuffix(version, "-test") {
		if k := os.Getenv("RUBI_TEST_ANNOUNCE_PUB"); k != "" {
			return k
		}
	}
	return AnnouncePub
}

// Announcement is the content of an announcement event.
type Announcement struct {
	Core    string `json:"core"`    // latest Rubi release
	Catalog string `json:"catalog"` // SHA-256 of the plugin catalog
	At      int64  `json:"-"`       // event time
}

// Announcements follows announcements from one key and calls On for each new one.
type Announcements struct {
	Key string
	On  func(Announcement)

	mu   sync.Mutex
	last int64
}

func (a *Announcements) filter() map[string]any {
	return map[string]any{"kinds": []int{AnnounceKind}, "authors": []string{a.Key}, "#d": []string{AnnounceTag}}
}

func (a *Announcements) handle(e *Event) {
	if e.PubKey != a.Key || Verify(e) != nil {
		return
	}
	d := ""
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == "d" {
			d = t[1]
		}
	}
	if d != AnnounceTag || e.CreatedAt > time.Now().Add(5*time.Minute).Unix() {
		return
	}
	var ann Announcement
	if json.Unmarshal([]byte(e.Content), &ann) != nil {
		return
	}
	ann.At = e.CreatedAt
	a.mu.Lock()
	if e.CreatedAt <= a.last {
		a.mu.Unlock()
		return
	}
	a.last = e.CreatedAt
	a.mu.Unlock()
	if a.On != nil {
		a.On(ann)
	}
}

// Publish signs an announcement and sends it to relays; it returns how many relays accepted it.
func Publish(ctx context.Context, key *Key, relays []string, ann Announcement) (int, error) {
	content, err := json.Marshal(ann)
	if err != nil {
		return 0, err
	}
	e := &Event{CreatedAt: time.Now().Unix(), Kind: AnnounceKind, Tags: [][]string{{"d", AnnounceTag}}, Content: string(content)}
	if err := key.Sign(e); err != nil {
		return 0, err
	}
	msg, _ := json.Marshal([]any{"EVENT", e})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, url := range relays {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			ws, _, err := websocket.Dial(cctx, url, nil)
			if err != nil {
				return
			}
			defer ws.CloseNow()
			if ws.Write(cctx, websocket.MessageText, msg) != nil {
				return
			}
			for {
				_, data, err := ws.Read(cctx)
				if err != nil {
					return
				}
				var r []json.RawMessage
				if json.Unmarshal(data, &r) == nil && len(r) >= 3 && string(r[0]) == `"OK"` && string(r[2]) == "true" {
					mu.Lock()
					ok++
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if ok == 0 {
		return 0, errors.New("no relay accepted the announcement")
	}
	return ok, nil
}
