//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

func terminalWidthFromOS() int {
	type winsize struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}
	var size winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stderr.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.col)
}
