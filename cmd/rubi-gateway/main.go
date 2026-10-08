// Command rubi-gateway runs Rubi Gateway, a relay that carries only Rubi's end-to-end encrypted panel
// traffic and release announcements (see internal/gateway). It runs behind Cloudflare Tunnel at
// wss://gateway.rubi-panel.com (docs/ops/gateway.md); behind another proxy, name the header that carries
// the client's address with -client-ip-header.
//
//	rubi-gateway -listen 127.0.0.1:7447
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/gateway"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7447", "address to listen on (behind a TLS proxy)")
	ipHeader := flag.String("client-ip-header", "CF-Connecting-IP", "header with the client's address, set by the local proxy")
	flag.Parse()
	s := gateway.New(relay.AnnouncePub)
	s.ClientIPHeader = *ipHeader
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	log.Printf("Rubi Gateway listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
