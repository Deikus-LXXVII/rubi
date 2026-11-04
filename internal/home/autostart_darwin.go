package home

import (
	"context"
	"fmt"
	"path/filepath"
)

// platformAutostart is the macOS form: a LaunchAgent in ~/Library/LaunchAgents loaded into the user's gui domain. It does
// what the app did before this stage, call for call.
func platformAutostart(env AutostartEnv) Autostart {
	domain := fmt.Sprintf("gui/%d", env.UID)
	target := domain + "/" + AgentLabel
	plistPath := filepath.Join(env.Home, "Library", "LaunchAgents", AgentLabel+".plist")
	launch := func(ctx context.Context, args ...string) error {
		_, err := env.run(ctx, "", launchctl, args...)
		return err
	}
	return autostartFuncs{
		install: func(ctx context.Context, spec AutostartSpec) (AutostartResult, error) {
			plistChanged, err := writeAgent(plistPath, spec.Bin, spec.Logs, env.Logf)
			if err != nil {
				return AutostartResult{}, fmt.Errorf("%w: %w", errAutostartWrite, err)
			}
			loaded, restarted, err := loadAgent(ctx, env, launch, plistPath, spec.BinChanged, plistChanged)
			return AutostartResult{WasLoaded: loaded, Restarted: restarted}, err
		},
		restart: func(ctx context.Context) error { return launch(ctx, "kickstart", "-k", target) },
		remove: func(ctx context.Context) error {
			_ = launch(ctx, "bootout", domain, plistPath)
			return removeFile(plistPath)
		},
		loaded: func(ctx context.Context) bool { return launch(ctx, "print", target) == nil },
	}
}
