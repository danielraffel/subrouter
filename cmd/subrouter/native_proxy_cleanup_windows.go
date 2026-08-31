//go:build windows

package main

import "os"

func removePrivateProxyHome(path string) {
	_ = os.Chmod(path, 0o700)
	_ = os.RemoveAll(path)
}
