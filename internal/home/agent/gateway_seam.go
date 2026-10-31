package agent

import "net/http"

// testGateway replaces the gateway's upstream in tests. It is unexported and nil in production: only code in this
// package (its tests, and the build-tagged hook in gateway_testhook.go) can set it, never a configuration.
var testGateway struct {
	upstream string
	client   *http.Client
}

func gatewaySeam() (string, *http.Client) { return testGateway.upstream, testGateway.client }
