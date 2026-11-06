package microvm

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestVirtiofsdRules(t *testing.T) {
	spec := roleSpec()
	bin := "/opt/rubi/qemu/bin/virtiofsd"
	libs := []string{"/opt/rubi/qemu/lib"}
	for i, s := range spec.Shares {
		got, err := VirtiofsdRules(bin, spec, i, libs)
		if err != nil {
			t.Fatal(err)
		}
		share := AccessWrite
		if s.ReadOnly {
			share = AccessRead
		}
		want := []FSRule{
			{s.Source, share},
			{spec.Dir, AccessWrite},
			{bin, AccessExec},
			{"/proc", AccessRead},
			{"/opt/rubi/qemu/lib", AccessExec},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("share %d:\n%v\nwant\n%v", i, got, want)
		}
		// nothing but the share's own folder is writable apart from the VM's folder, and nothing writable runs
		for _, r := range got {
			if r.Access == AccessWrite && r.Path != spec.Dir && r.Path != s.Source {
				t.Errorf("share %d: %s writable", i, r.Path)
			}
		}
	}
}

func TestVirtiofsdRulesRefuse(t *testing.T) {
	spec := roleSpec()
	for name, c := range map[string]struct {
		bin  string
		i    int
		libs []string
	}{
		"relative virtiofsd":     {"virtiofsd", 0, nil},
		"no such share":          {"/opt/v", 4, nil},
		"relative library":       {"/opt/v", 0, []string{"lib"}},
		"the root as a library":  {"/opt/v", 0, []string{"/"}},
		"unclean library folder": {"/opt/v", 0, []string{"/opt/../lib"}},
	} {
		if r, err := VirtiofsdRules(c.bin, spec, c.i, c.libs); !errors.Is(err, ErrSpec) {
			t.Errorf("%s: accepted, %v", name, r)
		}
	}
}

// A rule set that would leave the confinement open is refused before anything is confined or run.
func TestExecConfinedRefusesOpenRules(t *testing.T) {
	d := t.TempDir()
	for name, rules := range map[string][]FSRule{
		"the whole computer": {{"/", AccessRead}},
		"a missing path":     {{filepath.Join(d, "nope"), AccessRead}},
		"a relative path":    {{"tmp", AccessRead}},
		"no access":          {{d, 0}},
	} {
		if err := ExecConfined(rules, []string{"/bin/true"}, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ExecConfined([]FSRule{{d, AccessRead}}, nil, nil); err == nil {
		t.Error("nothing to run: accepted")
	}
}
