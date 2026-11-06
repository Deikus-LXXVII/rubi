//go:build !darwin

package home

// osascriptArgs is the macOS administrator run (elevate_darwin.go); there is none here.
func osascriptArgs(InstallPlan) []string { return nil }

// adminPrompt is the text of the macOS administrator dialog; it is empty where there is none.
const adminPrompt = ""
