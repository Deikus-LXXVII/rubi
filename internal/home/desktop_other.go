//go:build !darwin && !linux && !windows

package home

// platformDesktop on another system is the Linux form: freedesktop tools, if there are any.
func platformDesktop(env DesktopEnv) Desktop { return xdgDesktop(env) }
