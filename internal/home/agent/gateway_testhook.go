//go:build gatewaytest

package agent

import "net/http"

// SetTestGatewayUpstream points every auth gateway at a stand-in for api.anthropic.com. It exists only when built
// with -tags gatewaytest (the live check in internal/home); a production build has no such function.
func SetTestGatewayUpstream(url string, c *http.Client) {
	testGateway.upstream, testGateway.client = url, c
}
