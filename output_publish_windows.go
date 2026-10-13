//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func publishOutputFileNoReplace(tmp, output string) error {
	// os.Rename uses MOVEFILE_REPLACE_EXISTING on Windows. Omit that flag
	// so a destination created after our snapshot check remains untouched.
	return moveOutputFile(tmp, output, moveFileWriteThrough)
}

func replaceOutputFile(tmp, output string) error {
	return moveOutputFile(tmp, output, moveFileReplaceExisting|moveFileWriteThrough)
}

func moveOutputFile(tmp, output string, flags uintptr) error {
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
		flags,
	)
	if ok == 0 {
		return callErr
	}
	return nil
}
