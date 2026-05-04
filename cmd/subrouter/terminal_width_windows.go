//go:build windows

package main

import (
	"io"
	"os"
	"strconv"
)

func terminalColumns(_ io.Writer) int {
	value := os.Getenv("COLUMNS")
	if value == "" {
		return 0
	}
	columns, err := strconv.Atoi(value)
	if err != nil || columns <= 0 {
		return 0
	}
	return columns
}
