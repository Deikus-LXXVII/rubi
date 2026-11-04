//go:build !darwin

package home

import (
	"context"
	"errors"
)

// There is no Mac app on this system: these are the functions of app_darwin.go, and they say no.

// IsAppLaunch is false: nothing here is started from an app bundle.
func IsAppLaunch([]string, string) bool { return false }

// InAppBundle is false: there are no app bundles.
func InAppBundle(string) bool { return false }

func bundleID(string) string { return "" }

var errNoMacApp = errors.New("the LaunchAgent is for a Mac")

// AgentPlist is the LaunchAgent file; it exists on a Mac only.
func AgentPlist(string, string) (string, error) { return "", errNoMacApp }

func writeAgent(string, string, string, func(string, ...any)) (bool, error) {
	return false, errNoMacApp
}

func loadAgent(context.Context, AutostartEnv, func(context.Context, ...string) error, string, bool, bool) (bool, bool, error) {
	return false, false, errNoMacApp
}
