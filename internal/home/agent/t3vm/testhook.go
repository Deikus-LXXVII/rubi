//go:build t3vmtest

package t3vm

import (
	"context"
	"net"
)

// SetTestAPI points every VM's claude at a scripted stand-in for the Messages API: env is added to each session's
// environment and dial serves the VM's 127.0.0.1:3129. It exists only when built with -tags t3vmtest (the live check
// in internal/home); a production build has no such function and Home never calls it.
func (o *Options) SetTestAPI(env map[string]string, dial func(ctx context.Context) (net.Conn, error)) {
	o.testEnv, o.testAPI = env, dial
}
