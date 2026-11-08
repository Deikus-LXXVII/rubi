package t3

import (
	"os"
	"strconv"
)

func procOf(pid int) (procStat, error) { return statIn("/proc", pid) }

func argvOf(pid int) ([]string, error) { return argvIn("/proc", pid) }

// groupOf lists the processes of process group pgid.
func groupOf(pgid int) []int {
	ents, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if st, err := statIn("/proc", pid); err == nil && st.PGID == pgid {
			out = append(out, pid)
		}
	}
	return out
}
