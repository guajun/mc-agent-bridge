//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// detach starts the daemon in its own session so it outlives the CLI.
func detachAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}
