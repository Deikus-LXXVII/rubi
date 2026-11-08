package daemon

import (
	"context"
	"log"
	"os"
	"os/user"

	"github.com/Deikus-LXXVII/rubi/internal/homestart"
)

// keepHome keeps the user's Rubi Home running while the daemon does, where it is installed (a shared Linux box has no
// service manager to do it; elsewhere `rubi-home start` sees the system's own start and does nothing). The home
// folder comes from the account database: the daemon's environment is the Bot session's.
func keepHome(ctx context.Context) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return
	}
	go homestart.Keep(ctx, homestart.Env{Home: u.HomeDir, UID: os.Getuid()}, log.Printf)
}
