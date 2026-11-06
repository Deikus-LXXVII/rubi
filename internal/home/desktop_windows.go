package home

import "golang.org/x/sys/windows"

// platformDesktop is the Windows form (desktop_winps.go). Its programs are taken from the System32 folder the system
// reports, never from PATH.
func platformDesktop(env DesktopEnv) Desktop {
	if env.SystemDir == "" {
		if d, err := windows.GetSystemDirectory(); err == nil {
			env.SystemDir = d
		}
	}
	return winDesktop(env)
}
