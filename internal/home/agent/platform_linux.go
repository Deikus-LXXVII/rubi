package agent

import (
	"os"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// linuxBackend is the microvm backend of this Rubi Home: its VM files are this release's, and its own program confines the
// VMs' file servers (vm-confine).
func linuxBackend(cfg BackendConfig) VMBackend {
	exe, err := os.Executable()
	if err != nil {
		return NoBackend("Rubi Home can't find its own program (" + err.Error() + ")")
	}
	return NewMicroVMBackend(cfg, version.Version, exe)
}
