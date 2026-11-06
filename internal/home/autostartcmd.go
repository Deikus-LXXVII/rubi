package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deikus-LXXVII/rubi/internal/home/hostpaths"
)

// RunInstallAutostart is `rubi-home install-autostart [--bin PATH] [--remove]`, the internal step the installer runs
// (panel/install-home.sh on Linux) so that the start at login is made in one place, by the code the app uses too. It
// writes the definition for the binary at bin (a file Rubi Home was installed to), loads it and starts Rubi Home, or with
// remove stops it and takes it away. It reads no configuration and asks for no administrator rights.
func RunInstallAutostart(ctx context.Context, as Autostart, homeDir, bin string, remove bool) error {
	if remove {
		return as.Remove(ctx)
	}
	if !filepath.IsAbs(bin) {
		return errors.New("the installed binary's path is not absolute")
	}
	if fi, err := os.Stat(bin); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file Rubi Home can start", bin)
	}
	logs := filepath.Join(hostpaths.HelperHome(homeDir), "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		return err
	}
	_, err := as.Install(ctx, AutostartSpec{Bin: bin, Logs: logs, BinChanged: true})
	return err
}
