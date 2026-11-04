//go:build !darwin && !linux && !windows

package home

// platformAutostart on another system is the Linux form: a systemd user unit, if there is systemd.
func platformAutostart(env AutostartEnv) Autostart { return systemdAutostart(env) }
