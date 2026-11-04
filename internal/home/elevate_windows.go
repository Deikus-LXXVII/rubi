package home

import "golang.org/x/sys/windows"

// platformElevator is the Windows form: UAC for the system's PowerShell (elevate_uac.go). The programs are those of the
// System32 folder the system reports.
func platformElevator(env ElevateEnv) Elevator {
	if env.SystemDir == "" {
		if d, err := windows.GetSystemDirectory(); err == nil {
			env.SystemDir = d
		}
	}
	return uacElevator(env)
}
