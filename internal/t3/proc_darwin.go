package t3

import "golang.org/x/sys/unix"

func procOf(pid int) (procStat, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return procStat{}, err
	}
	tv := kp.Proc.P_starttime
	return procStat{PPID: int(kp.Eproc.Ppid), PGID: int(kp.Eproc.Pgid), Start: uint64(tv.Sec)*1e6 + uint64(tv.Usec)}, nil
}

func argvOf(pid int) ([]string, error) {
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2(b)
}
