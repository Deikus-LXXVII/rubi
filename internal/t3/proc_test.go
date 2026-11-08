package t3

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseStat(t *testing.T) {
	line := "4242 (node (t3) x) S 17 4242 4242 0 -1 4194560 1 0 0 0 0 0 0 0 20 0 11 0 987654 1000 100 18446744073709551615"
	st, err := parseStat(line)
	if err != nil || st != (procStat{PPID: 17, PGID: 4242, Start: 987654}) {
		t.Errorf("parseStat = %+v, %v", st, err)
	}
	for _, bad := range []string{"", "4242 node S 1 2", "1 (x) S 1 2 3", "1 (x) S a 2 3 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 5 1 1"} {
		if _, err := parseStat(bad); err == nil {
			t.Errorf("parseStat(%q) accepted", bad)
		}
	}
}

func TestProcReadersUnderARoot(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "77")
	_ = os.MkdirAll(d, 0o700)
	_ = os.WriteFile(filepath.Join(d, "stat"), []byte("77 (n) S 5 77 77 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 31 1 1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(d, "cmdline"), []byte("/x/node\x00/x/t3.mjs\x00serve\x00--port\x00\x00"), 0o600)
	if st, err := statIn(root, 77); err != nil || st != (procStat{PPID: 5, PGID: 77, Start: 31}) {
		t.Errorf("statIn = %+v, %v", st, err)
	}
	argv, err := argvIn(root, 77)
	if want := []string{"/x/node", "/x/t3.mjs", "serve", "--port", ""}; err != nil || !reflect.DeepEqual(argv, want) {
		t.Errorf("argvIn = %q, %v", argv, err)
	}
	_ = os.WriteFile(filepath.Join(d, "cmdline"), nil, 0o600)
	if _, err := argvIn(root, 77); err == nil {
		t.Error("an empty command line (a zombie) is read as one")
	}
	if _, err := statIn(root, 78); err == nil {
		t.Error("a missing process is read")
	}
}

func procargs(argc uint32, rest string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, argc)
	return append(b, rest...)
}

func TestParseProcArgs2(t *testing.T) {
	b := procargs(3, "/usr/local/bin/node\x00\x00\x00\x00node\x00/x/t3.mjs\x00serve\x00HOME=/h\x00")
	argv, err := parseProcArgs2(b)
	if want := []string{"node", "/x/t3.mjs", "serve"}; err != nil || !reflect.DeepEqual(argv, want) {
		t.Errorf("parseProcArgs2 = %q, %v", argv, err)
	}
	for name, bad := range map[string][]byte{
		"short":     {1, 0},
		"no argc":   procargs(0, "/n\x00a\x00"),
		"truncated": procargs(3, "/n\x00a\x00b"),
		"no path":   procargs(1, "abc"),
	} {
		if _, err := parseProcArgs2(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
