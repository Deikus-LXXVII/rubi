package fsx

import (
	"os"
	"sort"
	"testing"
)

func pids(ps []Proc) []uint32 {
	var out []uint32
	for _, p := range ps {
		out = append(out, p.PID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func same(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A table with the processes of a command (root 100, created at 1000): a child, a grandchild, a great-grandchild, an unrelated
// process, and an orphan whose parent (pid 50, long dead) shares its id with a stranger created later.
func table() []Proc {
	return []Proc{
		{PID: 4, PPID: 0, Created: 1},
		{PID: 100, PPID: 4, Created: 1000},
		{PID: 110, PPID: 100, Created: 1100},
		{PID: 111, PPID: 110, Created: 1200},
		{PID: 112, PPID: 111, Created: 1300},
		{PID: 200, PPID: 4, Created: 900},
		{PID: 201, PPID: 200, Created: 950},
	}
}

func TestDescendantsFindsTheTreeAndNothingElse(t *testing.T) {
	got := pids(Descendants(table(), 100, 1000))
	if !same(got, []uint32{110, 111, 112}) {
		t.Fatalf("got %v", got)
	}
	if got := Descendants(table(), 100, 0); got != nil {
		t.Fatalf("an unknown root time found %v", got)
	}
	if got := Descendants(nil, 100, 1000); len(got) != 0 {
		t.Fatalf("empty table found %v", got)
	}
}

// The pid of a dead parent is reused: an orphan of the old process lists the pid as its parent, but it is older than the new
// holder of the pid, so it is not that process's child. Without the creation-time rule the orphan (and what hangs from it) would
// be taken for descendants and killed.
func TestDescendantsRefusesAnOrphanOfAReusedPid(t *testing.T) {
	tab := append(table(),
		Proc{PID: 300, PPID: 4, Created: 500},    // the orphan, made by a process that had pid 120 and is gone
		Proc{PID: 301, PPID: 300, Created: 600},  // its child
		Proc{PID: 120, PPID: 100, Created: 1500}, // a stranger who got pid 120 later, a real child of the root
	)
	// the orphan names pid 120 as its parent, and 120 is in the tree (a real child of the root)
	tab[len(tab)-3].PPID = 120
	got := pids(Descendants(tab, 100, 1000))
	if !same(got, []uint32{110, 111, 112, 120}) {
		t.Fatalf("got %v: the orphan (300) or its child (301) was taken as a descendant", got)
	}
	// the same, with the root's own id reused: the orphans of an old process with pid 100 are older than the root
	tab2 := []Proc{{PID: 100, PPID: 4, Created: 1000}, {PID: 400, PPID: 100, Created: 10}}
	if got := Descendants(tab2, 100, 1000); len(got) != 0 {
		t.Fatalf("an older orphan of the root's pid was taken: %v", got)
	}
	// a child with no creation time is never taken
	if got := Descendants([]Proc{{PID: 500, PPID: 100, Created: 0}}, 100, 1000); len(got) != 0 {
		t.Fatalf("an unknown time was taken: %v", got)
	}
}

func TestSameProcess(t *testing.T) {
	if !SameProcess(5, 5) || SameProcess(5, 6) || SameProcess(0, 0) || SameProcess(5, 0) {
		t.Fatal("SameProcess")
	}
}

// Reap terminates the descendants, checks each again on the open process (a pid that now belongs to another process is left
// alone), lets go of every handle it opened, and catches a process started between two looks at the table.
func TestReapChecksEachProcessAgainBeforeTerminating(t *testing.T) {
	now := map[uint32]int64{110: 1100, 111: 7777 /* reused since the table was read */, 112: 1300}
	opened, released := map[uint32]bool{}, map[uint32]bool{}
	var terminated []uint32
	open := func(pid uint32) (int64, func(bool), bool) {
		c, ok := now[pid]
		if !ok {
			return 0, nil, false
		}
		opened[pid] = true
		return c, func(kill bool) {
			released[pid] = true
			if kill {
				terminated = append(terminated, pid)
			}
		}, true
	}
	snapshot := func() []Proc { return table() }
	killed := Reap(snapshot, 100, 1000, 3, open)
	// 111 changed, so it is left alone, and so is what hangs below it only if the table said so: 112's parent is 111, which was
	// found from the table (not killed); 112 is still checked on its own and terminated
	sort.Slice(terminated, func(i, j int) bool { return terminated[i] < terminated[j] })
	if !same(terminated, []uint32{110, 112}) || len(killed) != 2 {
		t.Fatalf("terminated %v, killed %v", terminated, killed)
	}
	for pid := range opened {
		if !released[pid] {
			t.Errorf("the handle of %d was not let go of", pid)
		}
	}
	// without the second look the stranger would have been terminated: the check is the whole point
	if released[111] && containsPID(terminated, 111) {
		t.Fatal("pid 111 now belongs to another process and was terminated")
	}
}

func containsPID(l []uint32, p uint32) bool {
	for _, x := range l {
		if x == p {
			return true
		}
	}
	return false
}

func TestReapCatchesWhatStartsMeanwhileAndStopsWhenNothingIsNew(t *testing.T) {
	calls := 0
	snapshot := func() []Proc {
		calls++
		tab := []Proc{{PID: 100, PPID: 4, Created: 1000}, {PID: 110, PPID: 100, Created: 1100}}
		if calls >= 2 {
			tab = append(tab, Proc{PID: 120, PPID: 110, Created: 1200})
		}
		return tab
	}
	var terminated []uint32
	open := func(pid uint32) (int64, func(bool), bool) {
		created := map[uint32]int64{110: 1100, 120: 1200}[pid]
		return created, func(kill bool) {
			if kill {
				terminated = append(terminated, pid)
			}
		}, true
	}
	Reap(snapshot, 100, 1000, 10, open)
	if !same(append([]uint32(nil), terminated...), []uint32{110, 120}) {
		t.Fatalf("terminated %v", terminated)
	}
	if calls != 3 { // look 1 finds 110, look 2 finds 120, look 3 finds nothing new and stops
		t.Fatalf("the table was read %d times", calls)
	}
	// a process that can't be opened is skipped, not retried forever
	n := 0
	Reap(func() []Proc { n++; return table() }, 100, 1000, 5, func(uint32) (int64, func(bool), bool) { return 0, nil, false })
	if n != 1 {
		t.Fatalf("the table was read %d times when nothing could be opened", n)
	}
}

// OpenNoFollow never asks Windows to create-or-truncate in one step: that could empty the target of a link put at the path
// before the look. O_TRUNC is carried out afterwards, through the handle.
func TestNoFollowPlan(t *testing.T) {
	const (
		read, write, rw = 0x80000000, 0x40000000, 0x80000000 | 0x40000000
		appendData      = 0x4
		createNew       = 1
		openExisting    = 3
		openAlways      = 4
		createAlways    = 2
		truncateExist   = 5
	)
	for name, c := range map[string]struct {
		flag         int
		access, disp uint32
		truncate     bool
	}{
		"read":             {os.O_RDONLY, read, openExisting, false},
		"write":            {os.O_WRONLY, write, openExisting, false},
		"read-write":       {os.O_RDWR, rw, openExisting, false},
		"create":           {os.O_WRONLY | os.O_CREATE, write, openAlways, false},
		"create exclusive": {os.O_WRONLY | os.O_CREATE | os.O_EXCL, write, createNew, false},
		"create truncate":  {os.O_WRONLY | os.O_CREATE | os.O_TRUNC, write, openAlways, true},
		"truncate":         {os.O_WRONLY | os.O_TRUNC, write, openExisting, true},
		"append":           {os.O_WRONLY | os.O_APPEND, appendData, openExisting, false},
	} {
		access, disp, truncate, _ := NoFollowPlan(c.flag, 0o600)
		if access != c.access || disp != c.disp || truncate != c.truncate {
			t.Errorf("%s: access %#x disposition %d truncate %v", name, access, disp, truncate)
		}
		if disp == createAlways || disp == truncateExist {
			t.Errorf("%s: the open itself would truncate", name)
		}
	}
	if _, _, _, attrs := NoFollowPlan(os.O_WRONLY|os.O_CREATE, 0o400); attrs != 0x01 {
		t.Errorf("a read-only file is made with attributes %#x", attrs)
	}
}
