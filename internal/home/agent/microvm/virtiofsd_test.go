package microvm

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

var user = HostIDs{UID: 1000, GID: 1001}

func TestVirtiofsdArgs(t *testing.T) {
	spec := roleSpec()
	d := "/home/u/.local/state/rubi-home/vm/web-app/"
	got, err := VirtiofsdArgs("/opt/rubi/qemu/bin/virtiofsd", spec, 0, user)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/opt/rubi/qemu/bin/virtiofsd", "--socket-path", d + "fs0.sock", "--shared-dir", "/home/u/src/web-app",
		"--sandbox", "none", "--translate-uid", "map:10001:1000:1", "--translate-gid", "map:10001:1001:1", "--cache", "auto"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	// a read-only share is refused writes by virtiofsd as well as by the guest's mount
	for i, s := range spec.Shares {
		args, err := VirtiofsdArgs("/opt/rubi/qemu/bin/virtiofsd", spec, i, user)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(args, "--readonly") != s.ReadOnly {
			t.Errorf("share %d (%s): %q", i, s.Target, args)
		}
		if slices.Contains(args, "--xattr") || slices.Contains(args, "--posix-acl") {
			t.Errorf("share %d: %q", i, args)
		}
	}
}

func TestVirtiofsdArgsRefuse(t *testing.T) {
	spec := roleSpec()
	for name, c := range map[string]struct {
		bin string
		i   int
		ids HostIDs
	}{
		"relative virtiofsd": {"virtiofsd", 0, user},
		"no such share":      {"/opt/v", 4, user},
		"negative share":     {"/opt/v", -1, user},
		"as root":            {"/opt/v", 0, HostIDs{0, 0}},
		"root group":         {"/opt/v", 0, HostIDs{1000, 0}},
	} {
		if args, err := VirtiofsdArgs(c.bin, spec, c.i, c.ids); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: accepted, %q", name, args)
		}
	}
}
