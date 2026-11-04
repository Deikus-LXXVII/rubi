package home

// platformDesktop is the Linux form (desktop_xdg.go).
func platformDesktop(env DesktopEnv) Desktop { return xdgDesktop(env) }
