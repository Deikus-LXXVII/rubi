package main

import (
	"strings"
	"testing"
)

// The help is what a Bot reads (project rule 2, T-07): the development stand-in for the panel stays out of it.
func TestUsageLeavesOutTheDevelopmentPanel(t *testing.T) {
	if strings.Contains(usage, "dev-panel") || strings.Contains(strings.ToLower(usage), "from the terminal") {
		t.Fatal("rubi's help names the development panel or the terminal")
	}
}
