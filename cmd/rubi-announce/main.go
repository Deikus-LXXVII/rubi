// Command rubi-announce publishes a release announcement to the default relays (see internal/relay,
// announce.go). The site workflow runs it after each release and catalog change, so installed Rubis
// check for updates at once instead of at their next periodic check.
//
//	NOSTR_ANNOUNCE_KEY=<hex> rubi-announce -core v0.4.0 -catalog <sha256>
//	rubi-announce -genkey   (prints a new private key, then the public key on stderr)
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/relay"
)

func main() {
	core := flag.String("core", "", "latest Rubi release")
	catalog := flag.String("catalog", "", "SHA-256 of the plugin catalog")
	genkey := flag.Bool("genkey", false, "print a new private key (stdout) and its public key (stderr)")
	flag.Parse()
	if *genkey {
		k, err := relay.NewKey()
		if err != nil {
			fail(err)
		}
		fmt.Println(k.Hex())
		fmt.Fprintln(os.Stderr, "public key:", k.Public())
		return
	}
	k, err := relay.KeyFromHex(strings.TrimSpace(os.Getenv("NOSTR_ANNOUNCE_KEY")))
	if err != nil {
		fail(fmt.Errorf("NOSTR_ANNOUNCE_KEY: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := relay.Publish(ctx, k, relay.DefaultRelays, relay.Announcement{Core: *core, Catalog: *catalog})
	if err != nil {
		fail(err)
	}
	fmt.Printf("announced to %d of %d relays (key %s)\n", n, len(relay.DefaultRelays), k.Public())
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "rubi-announce:", err)
	os.Exit(1)
}
