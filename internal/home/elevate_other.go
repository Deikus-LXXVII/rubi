//go:build !darwin && !linux && !windows

package home

// platformElevator on another system is the Linux form.
func platformElevator(env ElevateEnv) Elevator {
	return pkexecElevator(env)
}
