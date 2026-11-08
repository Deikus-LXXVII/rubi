package panelapi_test

import (
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"net/http"
	"strings"
	"testing"
)

func TestPanelRejectsForeignBrowserOriginsBeforeTaskRPC(t *testing.T) {
	c, srv := setup(t)
	for _, tc := range []struct {
		origin string
		want   int
	}{{"https://attacker.invalid", http.StatusForbidden}, {"null", http.StatusForbidden}, {c.PanelOrigin, http.StatusBadRequest}, {"", http.StatusBadRequest}} {
		req, err := http.NewRequest("POST", srv.URL+panelapi.RPCPath, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("origin policy: got %d, want %d", resp.StatusCode, tc.want)
		}
	}
}
