package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func defaultProbeOptions() ProbeOptions {
	return ProbeOptions{
		Preset:            "4",
		TargetVMAF:        defaultTargetVMAF,
		FloorVMAF:         defaultFloorVMAF,
		MaxEncodedPercent: defaultMaxEncodedPercent,
		CheckWorkers:      4,
		SampleDuration:    20 * time.Second,
		StallTimeout:      10 * time.Minute,
	}
}

func extNoDot(path string) string {
	ext := filepath.Ext(path)
	return strings.TrimPrefix(ext, ".")
}

func filenameNoExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func outputPathFor(input string) string {
	dir := filepath.Dir(input)
	base := filenameNoExt(filepath.Base(input))
	return filepath.Join(dir, base+"_[e-av1].mkv")
}

func skipNameMatch(path string, patterns []string) (string, bool) {
	base := displayPath(path)
	for _, pattern := range patterns {
		// An empty repeated flag would otherwise match every filename through
		// strings.Contains. Treat it as inert so shell-expanded variables cannot
		// accidentally skip an entire batch.
		if pattern == "" {
			continue
		}
		if strings.Contains(base, pattern) {
			return pattern, true
		}
	}
	return "", false
}

func formatSkipNameMessage(path string, pattern string) string {
	return fmt.Sprintf("%s: name matched --skip-name %q, skipping", displayPath(path), pattern)
}

func reserveTempPath(dir string, pattern string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	// CreateTemp gives us a collision-free randomized name, then ffmpeg creates
	// the actual media file itself. Leaving the placeholder would make ffmpeg's
	// overwrite behavior and cleanup harder to reason about.
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func reserveFinalTempPath(input string) (string, string, error) {
	dir := filepath.Dir(input)
	base := filenameNoExt(filepath.Base(input))
	output := outputPathFor(input)
	tmp, err := reserveTempPath(dir, "."+base+"_[e-av1].*.tmp.mkv")
	if err != nil {
		return "", "", err
	}
	return tmp, output, nil
}

func reserveSampleTempPath(input string, dir string, sampleN int, crf float64) (string, error) {
	if dir == "" {
		dir = filepath.Dir(input)
	}
	base := filenameNoExt(filepath.Base(input))
	return reserveTempPath(dir, fmt.Sprintf(".%s.sample%d.*.crf%s.av1.mkv", base, sampleN, terseFloat(crf)))
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func fileSize(path string) int64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return stat.Size()
}

func appendLogFile(path, input, output string) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\n%s\t%s\n\n", humanBytes(fileSize(input)), input, humanBytes(fileSize(output)), output)
	return err
}

func nullOutputPath() string {
	if runtime.GOOS == "windows" {
		return "NUL"
	}
	return "/dev/null"
}
