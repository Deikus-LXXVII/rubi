//go:build !linux

package home

import (
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// liveMicroVMBackend: off Linux the live runs use Apple's container tool.
func liveMicroVMBackend(*testing.T, *Helper) agent.VMBackend { return nil }
