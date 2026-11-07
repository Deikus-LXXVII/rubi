package setup

import (
	"errors"
	"testing"
)

func TestAfterSignInStartsTheSelfTest(t *testing.T) {
	n := 0
	AfterSignIn(func() error { n++; return nil })
	if n != 1 {
		t.Fatalf("started %d times", n)
	}
	AfterSignIn(func() error { return errors.New("busy") })
	AfterSignIn(nil)
}
