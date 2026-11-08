package t3

func procOf(pid int) (procStat, error) { return statIn("/proc", pid) }

func argvOf(pid int) ([]string, error) { return argvIn("/proc", pid) }
