//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"syscall"
)

func removePrivateProxyHome(path string) {
	// The launched CLI may make its disposable home read-only. Reclaim the
	// directory through a no-follow descriptor before removing its contents.
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err == nil {
		_ = syscall.Fchmod(descriptor, 0o700)
		_ = syscall.Close(descriptor)
	}
	_ = os.RemoveAll(path)
}
