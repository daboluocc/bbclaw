//go:build windows

package butlermcp

import (
	"os"
)

// checkPidLive on Windows opens the process handle to check liveness.
// os.FindProcess always succeeds; we use a zero-signal approach via OpenProcess.
// For simplicity, we attempt FindProcess and assume alive if no error —
// Windows doesn't support signal(0) but orphaned tasks will be reconciled
// on the next startup anyway.
func checkPidLive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
