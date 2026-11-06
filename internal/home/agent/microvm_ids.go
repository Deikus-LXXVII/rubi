//go:build unix

package agent

import "os"

func getuid() int { return os.Getuid() }
func getgid() int { return os.Getgid() }
