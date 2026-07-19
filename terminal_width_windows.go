//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

type consoleCoord struct {
	x int16
	y int16
}

type consoleRect struct {
	left   int16
	top    int16
	right  int16
	bottom int16
}

type consoleScreenBufferInfo struct {
	size              consoleCoord
	cursorPosition    consoleCoord
	attributes        uint16
	window            consoleRect
	maximumWindowSize consoleCoord
}

var getConsoleScreenBufferInfo = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleScreenBufferInfo")

func terminalWidthFromOS() int {
	var info consoleScreenBufferInfo
	ok, _, _ := getConsoleScreenBufferInfo.Call(os.Stderr.Fd(), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0
	}
	return int(info.window.right-info.window.left) + 1
}
