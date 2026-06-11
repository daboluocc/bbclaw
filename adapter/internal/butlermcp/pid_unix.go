//go:build !windows

package butlermcp

import (
	"os"
	"syscall"
)

// checkPidLive sends signal 0 to the process to check if it is alive.
// On Unix, kill(pid, 0) returns nil if the process exists, ESRCH if not.
func checkPidLive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
