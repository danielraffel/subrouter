//go:build windows

package main

import "os"

// processAlive reports whether pid names a running process. On Windows,
// FindProcess opens a handle to the process and fails when none exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = process.Release()
	return true
}
