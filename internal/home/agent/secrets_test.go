package agent

import (
	"testing"
)

func TestLooksSecret(t *testing.T) {
	for _, s := range []string{
		"x-api-key: sk-ant-api03-abc", "Authorization: Bearer abc", "GET /cb?code=abc", "refresh it", "my Token",
		"a1B2c3D4e5F6g7H8i9J0k1L2m3N4o5P6", "abcd.efgh_ijkl-mnop#qrst~uvwx+yz0123456789==",
		"1a2b3c4d.5e6f7g8h-9i0j1k2l_3m4n5o6p7q8r9",
	} {
		if !LooksSecret(s) {
			t.Errorf("%q passed", s)
		}
	}
	for _, s := range []string{"npm error network request failed", "Downloaded 31 of 148 MB", "#5 [builder 2/4] RUN npm ci --ignore-scripts"} {
		if LooksSecret(s) {
			t.Errorf("%q dropped", s)
		}
	}
}
