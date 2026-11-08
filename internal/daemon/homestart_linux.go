package daemon

import (
	"context"
	"log"
	"os"

	"github.com/Deikus-LXXVII/rubi/internal/homestart"
)

// keepHome keeps the user's Rubi Home running while the daemon does, where it is installed (a shared Linux box has no
// service manager to do it; elsewhere `rubi-home start` sees the system's own start and does nothing).
func keepHome(ctx context.Context) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	go homestart.Keep(ctx, homestart.Env{Home: home, UID: os.Getuid()}, log.Printf)
}
