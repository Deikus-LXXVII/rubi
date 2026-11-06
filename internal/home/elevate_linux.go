package home

// platformElevator is the Linux form: pkexec (elevate_pkexec.go).
func platformElevator(env ElevateEnv) Elevator {
	return pkexecElevator(env)
}
