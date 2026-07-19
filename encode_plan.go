package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type encodePathRef struct {
	path string
	key  string
	info os.FileInfo
}

func validateEncodePaths(opts EncodeOptions, files []string, actionable []eligibleInput) error {
	inputs := make([]encodePathRef, 0, len(files))
	for _, file := range files {
		ref, err := newEncodePathRef(file)
		if err != nil {
			return fmt.Errorf("inspect input %s: %w", displayPath(file), err)
		}
		inputs = append(inputs, ref)
	}

	actionableRefs := make([]encodePathRef, 0, len(actionable))
	outputs := make([]encodePathRef, 0, len(actionable))
	for _, input := range actionable {
		source, err := newEncodePathRef(input.File)
		if err != nil {
			return fmt.Errorf("inspect input %s: %w", displayPath(input.File), err)
		}
		for _, previous := range actionableRefs {
			if encodePathsAlias(source, previous) {
				return fmt.Errorf("inputs refer to the same file: %s and %s", displayPath(previous.path), displayPath(source.path))
			}
		}
		actionableRefs = append(actionableRefs, source)

		outputPath := outputPathFor(input.File)
		output, err := newEncodePathRef(outputPath)
		if err != nil {
			return fmt.Errorf("inspect output %s: %w", displayPath(outputPath), err)
		}
		for _, previous := range outputs {
			if encodePathsAlias(output, previous) {
				return fmt.Errorf("multiple inputs would write the same output: %s", displayPath(outputPath))
			}
		}
		for _, sourceInput := range inputs {
			if encodePathsAlias(output, sourceInput) {
				return fmt.Errorf("output %s conflicts with input %s", displayPath(outputPath), displayPath(sourceInput.path))
			}
		}
		outputs = append(outputs, output)
	}

	if opts.LogFile == "" {
		return nil
	}
	logFile, err := newEncodePathRef(opts.LogFile)
	if err != nil {
		return fmt.Errorf("inspect log file %s: %w", displayPath(opts.LogFile), err)
	}
	for _, input := range inputs {
		if encodePathsAlias(logFile, input) {
			return fmt.Errorf("log file %s conflicts with input %s", displayPath(opts.LogFile), displayPath(input.path))
		}
	}
	for _, output := range outputs {
		if encodePathsAlias(logFile, output) {
			return fmt.Errorf("log file %s conflicts with output %s", displayPath(opts.LogFile), displayPath(output.path))
		}
	}
	return nil
}

func newEncodePathRef(path string) (encodePathRef, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return encodePathRef{}, err
	}
	abs = filepath.Clean(abs)
	info, statErr := os.Stat(abs)
	if statErr != nil && !os.IsNotExist(statErr) {
		return encodePathRef{}, statErr
	}

	resolved := abs
	if statErr == nil {
		if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = evaluated
		}
	} else if evaluated, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		resolved = filepath.Join(evaluated, filepath.Base(abs))
	}
	resolved = filepath.Clean(resolved)
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}
	return encodePathRef{path: path, key: resolved, info: info}, nil
}

func encodePathsAlias(a, b encodePathRef) bool {
	if a.key == b.key {
		return true
	}
	return a.info != nil && b.info != nil && os.SameFile(a.info, b.info)
}
