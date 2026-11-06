package home

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// The Linux form of Autostart: a systemd user unit. It is what panel/install-home.sh used to write itself, call for
// call: the same unit file, then `systemctl --user daemon-reload`, `enable --now rubi-home.service`, `restart
// rubi-home.service`. The script now runs `rubi-home install-autostart`, so this is the one place the unit is made.

const systemdUnitName = "rubi-home.service"

// systemdUnit is the unit file for the installed binary. The text is the script's, byte for byte, for a plain path; a
// path with a space or another character that systemd reads specially is quoted (the script's unit broke on those).
func systemdUnit(bin string) (string, error) {
	q, err := systemdQuote(bin)
	if err != nil {
		return "", err
	}
	return "[Unit]\nDescription=Rubi Home\nAfter=network-online.target\n\n[Service]\nExecStart=" + q + " run\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=default.target\n", nil
}

func systemdQuote(p string) (string, error) {
	if p == "" || !strings.HasPrefix(p, "/") || strings.ContainsFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", errors.New("the path of the installed Rubi Home is not one a unit can run")
	}
	if !strings.ContainsAny(p, " \t\"'\\%$;") {
		return p, nil
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
	return `"` + r.Replace(p) + `"`, nil
}

// systemdAutostart makes the form for a user's systemd.
func systemdAutostart(env AutostartEnv) Autostart {
	unitPath := filepath.Join(env.Home, ".config", "systemd", "user", systemdUnitName)
	ctl := func(ctx context.Context, args ...string) error {
		name, err := env.LookPath("systemctl")
		if err != nil {
			return errors.New("this computer has no systemd to start Rubi Home at login")
		}
		out, err := env.run(ctx, "", name, append([]string{"--user"}, args...)...)
		if err != nil {
			return fmt.Errorf("systemd (user) %s: %w%s", strings.Join(args, " "), err, systemdHint(out+stderrOf(err)))
		}
		return nil
	}
	return autostartFuncs{
		install: func(ctx context.Context, spec AutostartSpec) (AutostartResult, error) {
			unit, err := systemdUnit(spec.Bin)
			if err != nil {
				return AutostartResult{}, fmt.Errorf("%w: %w", errAutostartWrite, err)
			}
			if _, err := env.LookPath("systemctl"); err != nil {
				return AutostartResult{}, errors.New("this computer has no systemd to start Rubi Home at login")
			}
			if err := writeFileReplace(unitPath, []byte(unit), 0o644); err != nil {
				return AutostartResult{}, fmt.Errorf("%w: %w", errAutostartWrite, err)
			}
			for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", systemdUnitName}, {"restart", systemdUnitName}} {
				if err := ctl(ctx, args...); err != nil {
					return AutostartResult{}, err
				}
			}
			return AutostartResult{Restarted: true}, nil
		},
		restart: func(ctx context.Context) error { return ctl(ctx, "restart", systemdUnitName) },
		remove: func(ctx context.Context) error {
			err := ctl(ctx, "disable", "--now", systemdUnitName)
			if rerr := removeFile(unitPath); rerr != nil && err == nil {
				err = rerr
			}
			_ = ctl(ctx, "daemon-reload")
			return err
		},
		loaded: func(ctx context.Context) bool { return ctl(ctx, "is-active", "--quiet", systemdUnitName) == nil },
	}
}

// systemdHint helps with the one failure that is not a bug: no user manager to talk to (a session with no login).
func systemdHint(out string) string {
	if strings.Contains(out, "Failed to connect to bus") || strings.Contains(out, "XDG_RUNTIME_DIR") {
		return " (there is no user session to talk to; log in to this computer once, or run loginctl enable-linger for this user)"
	}
	return ""
}
