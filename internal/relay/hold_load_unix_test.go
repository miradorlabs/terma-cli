//go:build unix

package relay

import (
	"syscall"
	"time"
)

// cpuTime is the process's user and system time so far.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
