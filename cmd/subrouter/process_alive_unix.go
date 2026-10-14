//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a running process. A process owned
// by another user still exists, so EPERM counts as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
