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
	for i := range actionable {
		input := &actionable[i]
		source, err := newEncodePathRef(input.File)
		if err != nil {
			return fmt.Errorf("inspect input %s: %w", displayPath(input.File), err)
		}
		if source.info == nil {
			return fmt.Errorf("inspect input %s: file no longer exists", displayPath(input.File))
		}
		input.Source = snapshotSource(source.info)
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
		input.Output = outputPath
		input.Destination = snapshotDestination(output.info)
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

type sourceSnapshot struct {
	info    os.FileInfo
	size    int64
	modTime int64
}

type destinationSnapshot struct {
	existed bool
	info    os.FileInfo
	size    int64
	modTime int64
}

func snapshotDestination(info os.FileInfo) destinationSnapshot {
	if info == nil {
		return destinationSnapshot{}
	}
	return destinationSnapshot{existed: true, info: info, size: info.Size(), modTime: info.ModTime().UnixNano()}
}

func verifyDestinationSnapshot(path string, snapshot destinationSnapshot, overwrite bool) error {
	info, err := os.Stat(path)
	if !snapshot.existed {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect output %s: %w", displayPath(path), err)
		}
		return fmt.Errorf("output appeared after planning: %s", displayPath(path))
	}
	if !overwrite {
		return fmt.Errorf("output already exists: %s", displayPath(path))
	}
	if err != nil {
		return fmt.Errorf("output changed after planning: %s: %w", displayPath(path), err)
	}
	if !os.SameFile(info, snapshot.info) || info.Size() != snapshot.size || info.ModTime().UnixNano() != snapshot.modTime {
		return fmt.Errorf("output changed after planning: %s", displayPath(path))
	}
	return nil
}

func acquireOutputCommitLock(output string) (string, error) {
	lock := filepath.Join(filepath.Dir(output), "."+filepath.Base(output)+".reencode-lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("output commit is locked: %s (if no matching reencode is active, remove the stale lock)", displayPath(lock))
		}
		return "", fmt.Errorf("lock output %s: %w", displayPath(output), err)
	}
	return lock, nil
}

func publishOutput(tmp, output string, destination destinationSnapshot, overwrite bool) error {
	if destination.existed {
		if !overwrite {
			return fmt.Errorf("output already exists: %s", displayPath(output))
		}
		if err := os.Rename(tmp, output); err != nil {
			return fmt.Errorf("replace output %s: %w", displayPath(output), err)
		}
		return nil
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(tmp, output); err != nil {
			return fmt.Errorf("publish output %s: %w", displayPath(output), err)
		}
		return nil
	}
	if err := os.Link(tmp, output); err != nil {
		return fmt.Errorf("publish output %s without replacement: %w", displayPath(output), err)
	}
	if err := os.Remove(tmp); err != nil {
		return fmt.Errorf("remove committed temporary output %s: %w", displayPath(tmp), err)
	}
	return nil
}

func snapshotSource(info os.FileInfo) sourceSnapshot {
	return sourceSnapshot{info: info, size: info.Size(), modTime: info.ModTime().UnixNano()}
}

func sourceMatchesSnapshot(info os.FileInfo, snapshot sourceSnapshot) bool {
	return snapshot.info != nil && os.SameFile(info, snapshot.info) && info.Size() == snapshot.size && info.ModTime().UnixNano() == snapshot.modTime
}

func verifySourceSnapshot(path string, snapshot sourceSnapshot) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("source changed since eligibility check: %s: %w", displayPath(path), err)
	}
	if !sourceMatchesSnapshot(info, snapshot) {
		return fmt.Errorf("source changed since eligibility check: %s", displayPath(path))
	}
	return nil
}

func quarantineSource(path string, snapshot sourceSnapshot) (string, error) {
	dir := filepath.Dir(path)
	pattern := "." + filepath.Base(path) + ".reencode-source.*.pending-remove"
	quarantine, err := reserveTempPath(dir, pattern)
	if err != nil {
		return "", err
	}
	if err := os.Rename(path, quarantine); err != nil {
		return "", fmt.Errorf("quarantine source %s: %w", displayPath(path), err)
	}
	info, err := os.Stat(quarantine)
	if err == nil && sourceMatchesSnapshot(info, snapshot) {
		return quarantine, nil
	}
	mismatch := fmt.Errorf("source changed before removal: %s", displayPath(path))
	if err != nil {
		mismatch = fmt.Errorf("source changed before removal: %s: %w", displayPath(path), err)
	}
	if restoreErr := restoreQuarantinedSource(quarantine, path); restoreErr != nil {
		return "", fmt.Errorf("%w; replacement retained as %s because restoration failed: %v", mismatch, displayPath(quarantine), restoreErr)
	}
	return "", mismatch
}

func restoreQuarantinedSource(quarantine, original string) error {
	if _, err := os.Lstat(original); err == nil {
		return fmt.Errorf("original path is occupied")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Linking before unlinking gives restoration no-replace behavior on Unix
	// and Windows. If the filesystem cannot create hard links, retain the
	// quarantine path rather than risk replacing an unrelated file.
	if err := os.Link(quarantine, original); err != nil {
		return err
	}
	if err := os.Remove(quarantine); err != nil {
		_ = os.Remove(original)
		return err
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
