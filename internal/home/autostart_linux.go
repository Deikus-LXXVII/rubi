package home

// platformAutostart is the Linux form: a systemd user unit (autostart_systemd.go).
func platformAutostart(env AutostartEnv) Autostart { return systemdAutostart(env) }
