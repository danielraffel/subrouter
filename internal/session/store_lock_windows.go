//go:build windows

package session

import (
	"os"
	"path/filepath"
)

type storeFileLock struct {
	file *os.File
}

func lockSessionStore(path string) (*storeFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &storeFileLock{file: file}, nil
}

func (l *storeFileLock) Close() error {
	return l.file.Close()
}
