// Command rubi-gateway runs Rubi Gateway, a relay that carries only Rubi's end-to-end encrypted panel
// traffic and release announcements (see internal/gateway). It runs behind Cloudflare Tunnel at
// wss://gateway.rubi-panel.com (docs/ops/gateway.md); behind another proxy, name the header that carries
// the client's address with -client-ip-header.
//
//	rubi-gateway -listen 127.0.0.1:7447
package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/gateway"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7447", "address to listen on (behind a TLS proxy)")
	ipHeader := flag.String("client-ip-header", "CF-Connecting-IP", "header with the client's address, set by the local proxy")
	stateDir := flag.String("state-dir", os.Getenv("STATE_DIRECTORY"), "where the watch key is kept (systemd StateDirectory)")
	flag.Parse()
	s := gateway.New(relay.AnnouncePub)
	s.ClientIPHeader = *ipHeader
	// The watch key must outlive restarts: Rubis keep registrations sealed to it.
	key, err := watchKey(*stateDir)
	if err != nil {
		log.Fatal(err)
	}
	s.Watch = watch.New(key)
	go s.Watch.Run(context.Background())
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	log.Printf("Rubi Gateway listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}

// watchKey loads the gateway's watch key from dir, creating it on first start ("" keeps it in memory).
func watchKey(dir string) (*ecdh.PrivateKey, error) {
	if dir == "" {
		log.Printf("no -state-dir: the watch key lasts only until restart")
		return ecdh.X25519().GenerateKey(rand.Reader)
	}
	path := filepath.Join(dir, "watch.key")
	if b, err := os.ReadFile(path); err == nil {
		return ecdh.X25519().NewPrivateKey(b)
	}
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, k.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}
