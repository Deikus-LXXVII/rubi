// Command rubi-gateway runs Rubi Gateway, a relay that carries only Rubi's end-to-end encrypted panel
// traffic and release announcements (see internal/gateway). Put it behind a TLS proxy (for example Caddy)
// at wss://gateway.rubi-panel.com.
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
	flag.Parse()
	s := gateway.New(relay.AnnouncePub)
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("Rubi Gateway listening on %s", *listen)
	log.Fatal(srv.ListenAndServe())
}
