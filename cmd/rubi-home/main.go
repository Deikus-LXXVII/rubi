// Command rubi-home runs Rubi Home, the helper on a computer at home that lets Rubi control the Philips
// Hue Bridge and run the user's Shortcuts (see internal/home).
//
//	rubi-home run            serve Rubi's requests (the LaunchAgent runs this)
//	rubi-home pair           print a one-time code to paste into the Rubi panel
//	rubi-home status         show what is paired and allowed
//	rubi-home folder NAME    the Shortcuts folder Rubi may use (default: Rubi)
//	rubi-home unpair         forget every paired Rubi
//	rubi-home hue-forget ID  forget a Hue Bridge's recorded certificate (after replacing or resetting it)
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Deikus-LXXVII/rubi/internal/home"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/version"
	"github.com/Deikus-LXXVII/rubi/internal/watch"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	h, err := home.Open(home.DefaultDir())
	if err != nil {
		log.Fatal(err)
	}
	switch os.Args[1] {
	case "run":
		run(h)
	case "pair":
		code, err := h.StartPairing()
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("In the Rubi panel open Settings > Rubi Home > Add a home computer, and paste this code")
		fmt.Println("(it works once, within 15 minutes; Rubi Home must be running):")
		fmt.Println()
		fmt.Println(code.String())
	case "status":
		cfg := h.Config()
		fmt.Printf("Rubi Home %s on %q\n", version.Version, cfg.Name)
		sum := sha256.Sum256([]byte(h.ID.PublicBundle()))
		fmt.Printf("Fingerprint: %s\n", base64.RawURLEncoding.EncodeToString(sum[:6]))
		fmt.Printf("Paired with %d Rubi\n", len(cfg.Paired))
		for _, p := range cfg.Paired {
			fmt.Printf("  %s, since %s\n", p.Label, p.At.Local().Format("2006-01-02 15:04"))
		}
		fmt.Printf("Shortcuts folder: %s\n", cfg.ShortcutsFolder)
		fmt.Printf("Hue Bridges paired: %d\n", len(cfg.HuePins))
	case "folder":
		if len(os.Args) != 3 || strings.TrimSpace(os.Args[2]) == "" {
			usage()
		}
		if strings.EqualFold(strings.TrimSpace(os.Args[2]), "none") { // the Shortcuts CLI reads it as "no folder": every loose shortcut
			log.Fatal(`choose a real folder name, not "none"`)
		}
		if err := h.Update(func(c *home.Config) { c.ShortcutsFolder = os.Args[2] }); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Rubi may now use the shortcuts in the folder %q.\n", os.Args[2])
	case "unpair":
		if err := h.Update(func(c *home.Config) { c.Paired, c.Pending = nil, nil }); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Forgot every paired Rubi. Pair again with: rubi-home pair")
	case "hue-forget":
		if len(os.Args) != 3 {
			usage()
		}
		if err := h.Update(func(c *home.Config) { delete(c.HuePins, strings.ToLower(os.Args[2])) }); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Forgot that bridge. Set Hue up again in the Rubi panel.")
	case "version":
		fmt.Println(version.Version)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rubi-home run | pair | status | folder NAME | unpair | hue-forget ID | version")
	os.Exit(2)
}

func run(h *home.Helper) {
	key, err := h.RelayKey()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Watching the Rubi uses this helper's own key: Rubi seals its registration to it.
	h.Watch = watch.New(h.ID.Box)
	go h.Watch.Run(ctx)
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: h.Handle}
	relays := h.Relays()
	last := -1
	srv := &relay.Server{Key: key, Relays: relays, Handle: rpc.HandleRPC, Logf: log.Printf,
		Ready: func(n int) {
			if (n == 0) != (last == 0) || last < 0 {
				log.Printf("relays connected: %d of %d", n, len(relays))
			}
			last = n
		}}
	log.Printf("Rubi Home %s running; Shortcuts folder %q", version.Version, h.Config().ShortcutsFolder)
	srv.Run(ctx)
}
