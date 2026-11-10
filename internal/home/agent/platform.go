package agent

import "errors"

var errNoBackend = errors.New("VM agents were retired; use T3 host sandbox sessions")

// All execution is now via native T3 and the per-project host sandbox.
func platformBackend(BackendConfig) VMBackend {
	return NoBackend("VM agents were retired; use T3 host sandbox sessions")
}
