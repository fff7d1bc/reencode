//go:build !windows

package main

import "os"

func publishOutputFileNoReplace(tmp, output string) error {
	if err := os.Link(tmp, output); err != nil {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return nil
}

func replaceOutputFile(tmp, output string) error {
	return os.Rename(tmp, output)
}
