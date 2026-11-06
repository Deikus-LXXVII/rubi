package setup

import (
	"context"
	"errors"
)

// verifyPackage checks the downloaded installer package of Apple's container tool the way macOS does: its signature
// (`pkgutil --check-signature`: signed by a Developer ID Installer certificate of the given team and notarized) and the
// system's assessment (`spctl`: accepted as a notarized Developer ID package of that team).
func verifyPackage(ctx context.Context, run Commander, pkg, team string) error {
	out, err := run.Run(ctx, []string{"/usr/sbin/pkgutil", "--check-signature", pkg})
	if err != nil {
		return errors.New("the signature check failed")
	}
	if err := checkSignature(string(out), team); err != nil {
		return err
	}
	out, err = run.Run(ctx, []string{"/usr/sbin/spctl", "--assess", "--type", "install", "-vv", pkg})
	if err != nil {
		return errors.New("macOS rejected the package")
	}
	return checkAssessment(string(out), team)
}
