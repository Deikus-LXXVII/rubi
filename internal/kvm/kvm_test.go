package kvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fake struct {
	usableAfterGrant bool
	granted          bool
	calls            [][]string
	runErr           error
	logs             []string
}

func (f *fake) opts(t *testing.T, grant bool) Options {
	dev := filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return Options{
		GrantWithSudo: grant,
		Device:        dev,
		Logf:          func(f2 string, a ...any) { f.logs = append(f.logs, f2) },
		usable: func(string) error {
			if f.granted && f.usableAfterGrant {
				return nil
			}
			return os.ErrPermission
		},
		run: func(_ context.Context, name string, args ...string) error {
			f.calls = append(f.calls, append([]string{name}, args...))
			if f.runErr != nil {
				return f.runErr
			}
			f.granted = true
			return nil
		},
		username: func() (string, error) { return "svc", nil },
		owner:    func(string) string { return "kvmgroup" },
	}
}

func TestEnsureGrantsWithSudoWhenAllowed(t *testing.T) {
	f := &fake{usableAfterGrant: true}
	o := f.opts(t, true)
	if err := Ensure(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"sudo", "-n", "setfacl", "-m", "u:svc:rw", o.Device}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if len(f.logs) != 1 || !strings.Contains(f.logs[0], "granted") {
		t.Errorf("the grant was not logged: %v", f.logs)
	}
}

func TestEnsureDoesNothingWhenUsable(t *testing.T) {
	f := &fake{granted: true, usableAfterGrant: true}
	if err := Ensure(context.Background(), f.opts(t, true)); err != nil || len(f.calls) != 0 {
		t.Errorf("err = %v, calls = %v", err, f.calls)
	}
}

func TestEnsureRefusesWithoutTheSetting(t *testing.T) {
	f := &fake{usableAfterGrant: true}
	err := Ensure(context.Background(), f.opts(t, false))
	if err == nil || len(f.calls) != 0 || !strings.Contains(err.Error(), "kvmgroup") || !strings.Contains(err.Error(), "setting") ||
		!strings.Contains(err.Error(), "agent") || strings.Contains(err.Error(), "sudo") {
		t.Errorf("err = %v, calls = %v", err, f.calls)
	}
}

func TestEnsureFailsClosedWithoutSudo(t *testing.T) {
	f := &fake{runErr: errors.New("sudo: a password is required")}
	err := Ensure(context.Background(), f.opts(t, true))
	if err == nil || !strings.Contains(err.Error(), "passwordless administrator") || !strings.Contains(err.Error(), "password is required") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureChecksAgainAfterTheGrant(t *testing.T) {
	f := &fake{usableAfterGrant: false}
	if err := Ensure(context.Background(), f.opts(t, true)); err == nil || !strings.Contains(err.Error(), "still not usable") {
		t.Errorf("err = %v", err)
	}
}

func TestEnsureRefusesAMissingDevice(t *testing.T) {
	f := &fake{}
	o := f.opts(t, true)
	o.Device = filepath.Join(t.TempDir(), "none")
	if err := Ensure(context.Background(), o); err == nil || !strings.Contains(err.Error(), "missing") || len(f.calls) != 0 {
		t.Errorf("err = %v, calls = %v", err, f.calls)
	}
}

func TestEnsureRefusesAnOddUserName(t *testing.T) {
	for _, name := range []string{"", "-x", "a:b", "a,b", "a b", "u:root:rw"} {
		f := &fake{usableAfterGrant: true}
		o := f.opts(t, true)
		o.username = func() (string, error) { return name, nil }
		if err := Ensure(context.Background(), o); err == nil || len(f.calls) != 0 {
			t.Errorf("user %q: err = %v, calls = %v", name, err, f.calls)
		}
	}
}
