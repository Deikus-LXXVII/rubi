package home

import (
	"os/user"

	"golang.org/x/sys/windows"
)

// platformAutostart is the Windows form: a Scheduled Task of the user (autostart_schtasks.go). The programs are those of
// the System32 folder the system reports, and the user is the account of this process.
func platformAutostart(env AutostartEnv) Autostart {
	if env.SystemDir == "" {
		if d, err := windows.GetSystemDirectory(); err == nil {
			env.SystemDir = d
		}
	}
	if env.User == "" {
		if u, err := user.Current(); err == nil {
			env.User = u.Username
		}
	}
	return schtasksAutostart(env)
}
