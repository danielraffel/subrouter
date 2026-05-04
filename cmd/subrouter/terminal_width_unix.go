//go:build !windows

package main

import (
	"io"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

type terminalWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func terminalColumns(out io.Writer) int {
	if columns := columnsFromEnv(); columns > 0 {
		return columns
	}
	file, ok := out.(*os.File)
	if !ok {
		return 0
	}
	var size terminalWinsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 || size.Col == 0 {
		return 0
	}
	return int(size.Col)
}

func columnsFromEnv() int {
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
