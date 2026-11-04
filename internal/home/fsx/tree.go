package fsx

// The decision logic of killing a process's descendants where there are no process groups (Windows). It is plain code over a
// table of processes so that every host can test it; the Windows file supplies the table and the way to terminate.
//
// A process table says who the parent of each process is, but not whether that parent is still the process it was: when a
// parent exits, its children keep its pid as their parent id, and Windows hands the pid out again. An orphan of a dead parent
// would then look like a child of whoever got the pid. Creation times settle it: a real child is created after its parent,
// so an entry is taken as a child only when it was created at or after its parent (the equality is for a clock that ticks
// more slowly than processes start; a pid taken over by a stranger is created after the orphans it "inherits", so they are
// always older). The check is made again on the open process just before it is terminated.

// Proc is one process of a table: its id, its parent's id and its creation time (any unit, increasing; 0 means unknown, and an
// unknown time is never taken as a descendant).
type Proc struct {
	PID, PPID uint32
	Created   int64
}

// Descendants are the processes of table that descend from root, which was created at rootCreated: the processes whose parent
// is root or an earlier-found descendant, and that were created at or after that parent. root itself is not in the result.
func Descendants(table []Proc, root uint32, rootCreated int64) []Proc {
	if rootCreated == 0 {
		return nil
	}
	in := map[uint32]int64{root: rootCreated}
	var out []Proc
	for changed := true; changed; {
		changed = false
		for _, p := range table {
			if p.PID == root || p.Created == 0 {
				continue
			}
			if _, done := in[p.PID]; done {
				continue
			}
			if pc, ok := in[p.PPID]; ok && p.Created >= pc {
				in[p.PID] = p.Created
				out = append(out, p)
				changed = true
			}
		}
	}
	return out
}

// SameProcess says whether a process opened now is the one the table showed: its creation time is the same, and known.
func SameProcess(listed, now int64) bool { return listed != 0 && listed == now }

// Reap terminates the descendants of root, going over the table up to rounds times to catch what was started meanwhile.
// snapshot reads the table. open opens a process by id for termination and returns its creation time (as the open process
// says it) and the function that terminates it; ok is false when it can't be opened (gone, or not ours). A process whose
// creation time now differs from the table's is a different process under a reused pid and is left alone. It returns the
// processes it terminated.
func Reap(snapshot func() []Proc, root uint32, rootCreated int64, rounds int,
	open func(pid uint32) (created int64, finish func(terminate bool), ok bool)) []Proc {
	var killed []Proc
	seen := map[Proc]bool{}
	for r := 0; r < rounds; r++ {
		fresh := false
		for _, p := range Descendants(snapshot(), root, rootCreated) {
			if seen[p] {
				continue
			}
			seen[p] = true
			created, finish, ok := open(p.PID)
			if !ok {
				continue
			}
			if !SameProcess(p.Created, created) {
				finish(false)
				continue
			}
			finish(true)
			killed = append(killed, p)
			fresh = true
		}
		if !fresh {
			break
		}
	}
	return killed
}
