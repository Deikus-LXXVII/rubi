//go:build !linux

package home

import (
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// liveMicroVMNewVM: off Linux the live runs use Apple's container tool.
func liveMicroVMNewVM(*testing.T, *Helper) func(agent.Project) agent.VM { return nil }
