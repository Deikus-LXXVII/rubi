package home

import "github.com/Deikus-LXXVII/rubi/internal/home/fsx"

// procExe is the full path of a process's program (QueryFullProcessImageName).
func procExe(pid int) (string, error) { return fsx.ProcessImage(pid) }
