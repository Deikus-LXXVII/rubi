//go:build rubi_acceptance

package agenttask

import (
	"net"
	"net/url"
	"path/filepath"
)

// AcceptancePublisher cannot exist in a release. Even this test seam permits
// only a local absolute bare repo and literal loopback HTTP GitHub fixture.
func AcceptancePublisher(store Store, helper, remote, api string) Publisher {
	u, err := url.Parse(api)
	if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || !filepath.IsAbs(remote) {
		panic("invalid local acceptance destination")
	}
	return Publisher{Store: store, Helper: helper, api: api, remote: func(Target) string { return remote }}
}
