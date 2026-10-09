package watch

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWatcher(t *testing.T) {
	hits := make(chan map[string]any, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer crsr_admin" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		hits <- m
	}))
	defer srv.Close()

	wk, _ := ecdh.X25519().GenerateKey(rand.Reader)
	w := New(wk)
	now := time.Now()
	w.Cfg.Now = func() time.Time { return now }
	clock = func() time.Time { return now }
	defer func() { clock = time.Now }()
	w.Cfg.Insecure, w.Cfg.Client = true, http.DefaultClient

	_, rubiKey, _ := ed25519.GenerateKey(rand.Reader)
	reg := Registration{WatchKey: b64.EncodeToString(rubiKey.Public().(ed25519.PublicKey)), Name: "rubi-1", Admin: "Main",
		URL: srv.URL, Key: "crsr_admin"}
	sealed, err := Seal(wk.PublicKey(), reg)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(kind string) {
		t.Helper()
		select {
		case m := <-hits:
			if m["type"] != "rubi.watch."+kind {
				t.Fatalf("got %v, want %s", m["type"], kind)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no %s notice", kind)
		}
	}
	none := func() {
		t.Helper()
		select {
		case m := <-hits:
			t.Fatalf("unexpected notice %v", m)
		case <-time.After(100 * time.Millisecond):
		}
	}
	beat := func(state, reason string) error { return w.Receive(NewMessage(rubiKey, state, reason, sealed)) }

	if err := beat("unlocked", ""); err != nil {
		t.Fatal(err)
	}
	// Forged beats and registrations for another watcher are refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if err := w.Receive(NewMessage(other, "unlocked", "", sealed)); err == nil {
		t.Fatal("a beat by another key used this registration")
	}
	ok, _ := ecdh.X25519().GenerateKey(rand.Reader)
	foreign, _ := Seal(ok.PublicKey(), reg)
	if err := w.Receive(NewMessage(rubiKey, "unlocked", "", foreign)); err == nil {
		t.Fatal("a registration sealed for another watcher was opened")
	}

	// Silence: "down", once; then a beat: "back".
	now = now.Add(6 * time.Minute)
	w.Check()
	expect("down")
	w.Check()
	none()
	if err := beat("locked", ""); err != nil {
		t.Fatal(err)
	}
	expect("back")
	// Locked for long: "locked", once.
	now = now.Add(4 * time.Minute)
	_ = beat("locked", "")
	w.Check()
	none()
	now = now.Add(7 * time.Minute)
	_ = beat("locked", "")
	w.Check()
	expect("locked")
	w.Check()
	none()
	// Stopping: told at once, and no "down" afterwards.
	_ = beat("stopping", "the computer is shutting down")
	expect("stopping")
	now = now.Add(10 * time.Minute)
	w.Check()
	none()
}
