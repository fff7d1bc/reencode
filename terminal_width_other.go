//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package main

func terminalWidthFromOS() int {
	return 0
}
