//go:build unix

package main

import (
	"syscall"
	"time"
)

// cpuTime is the process's user plus system CPU time; unlike wall time it stays comparable on a loaded machine.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
