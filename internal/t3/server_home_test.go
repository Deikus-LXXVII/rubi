package t3

import (
	"os"
	"strings"
	"testing"
)

func TestDisposableServerDoesNotDiscoverTheRealHome(t *testing.T) {
	s := &Server{Home: t.TempDir()}
	var actual string
	for _, v := range s.Env() {
		if strings.HasPrefix(v, "HOME=") {
			actual = strings.TrimPrefix(v, "HOME=")
		}
	}
	if actual != s.Home || actual == os.Getenv("HOME") {
		t.Fatal("disposable native server inherited the real profile")
	}
	if (&Server{}).home() != os.Getenv("HOME") {
		t.Fatal("existing server profile behavior changed")
	}
}
