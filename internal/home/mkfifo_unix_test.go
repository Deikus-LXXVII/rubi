//go:build unix

package home

import "syscall"

func mkfifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }
