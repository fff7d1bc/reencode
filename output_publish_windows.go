//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func publishOutputFileNoReplace(tmp, output string) error {
	return os.Rename(tmp, output)
}

func replaceOutputFile(tmp, output string) error {
	from, err := syscall.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(output)
	if err != nil {
		return err
	}
	ok, _, callErr := moveFileEx.Call(
		uintptr(unsafe.Pointer(from)),
		uintptr(unsafe.Pointer(to)),
		moveFileReplaceExisting|moveFileWriteThrough,
	)
	if ok == 0 {
		return callErr
	}
	return nil
}
