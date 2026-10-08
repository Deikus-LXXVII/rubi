package core

import (
	"context"
	"log"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/integrity"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// OnAnnouncement reacts to a release announcement from the relays: it checks for Rubi and plugin updates
// right away (bypassing CDN caches), and once more a little later in case the site was still deploying.
// The announcement itself is only a hint; the checks verify everything as usual.
func (c *Core) OnAnnouncement(a relay.Announcement) {
	log.Printf("release announcement: core %s, catalog %.12s", a.Core, a.Catalog)
	go func() {
		for _, wait := range []time.Duration{0, 2 * time.Minute} {
			time.Sleep(wait)
			ctx, cancel := context.WithTimeout(integrity.WithFresh(context.Background()), time.Minute)
			if a.Core != "" && a.Core != version.Version {
				c.CheckForUpdate(ctx)
			}
			c.CheckPluginUpdates(ctx)
			cancel()
		}
	}()
}
