//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

func executableIdentityForProcess(pid int) (processExecutableIdentity, error) {
	file, err := os.Open(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return processExecutableIdentity{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return processExecutableIdentity{}, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processExecutableIdentity{}, err
	}
	parts := strings.SplitN(string(stat), ") ", 2)
	if len(parts) != 2 {
		return processExecutableIdentity{}, fmt.Errorf("invalid process stat for pid %d", pid)
	}
	fields := strings.Fields(parts[1])
	if len(fields) < 20 {
		return processExecutableIdentity{}, fmt.Errorf("invalid process stat for pid %d", pid)
	}
	return processExecutableIdentity{Kind: "linux-proc-exe-sha256", Value: hex.EncodeToString(hash.Sum(nil)), StartIdentity: "linux:" + fields[19]}, nil
}
