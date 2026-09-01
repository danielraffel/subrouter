//go:build windows

package main

import "os"

func removePrivateProxyHome(path string) error {
	_ = os.Chmod(path, 0o700)
	return os.RemoveAll(path)
}
