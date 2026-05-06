package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

type eligibilityMode int

const (
	eligibilityProbe eligibilityMode = iota
	eligibilityEncode
)

type eligibilityReason string

const (
	eligibilitySkipName       eligibilityReason = "name filter"
	eligibilitySkipNotVideo   eligibilityReason = "not video"
	eligibilitySkipNoVideo    eligibilityReason = "no video"
	eligibilitySkipAlreadyAV1 eligibilityReason = "already AV1"
)

type eligibleInput struct {
	File string
	Info MediaInfo
}

type skippedInput struct {
	File    string
	Reason  eligibilityReason
	Pattern string
}

type eligibilityEntry struct {
	File       string
	Info       MediaInfo
	Skipped    bool
	SkipReason eligibilityReason
	Pattern    string
	Fatal      error
}

type eligibilityResult struct {
	Actionable []eligibleInput
	Skipped    []skippedInput
	Entries    []eligibilityEntry
	Fatal      error
}

// Keep the classifier injectable so the worker-pool ordering rules can be
// tested without constructing real media files or shelling out to ffprobe.
type eligibilityClassifier func(context.Context, eligibilityMode, string, EncodeOptions) eligibilityEntry

func collectEligibleInputs(ctx context.Context, mode eligibilityMode, files []string, opts EncodeOptions) eligibilityResult {
	return collectEligibleInputsWithClassifier(ctx, mode, files, opts, classifyEligibilityInput)
}

func collectEligibleInputsWithClassifier(ctx context.Context, mode eligibilityMode, files []string, opts EncodeOptions, classifier eligibilityClassifier) eligibilityResult {
	entries := make([]eligibilityEntry, len(files))
	workers := eligibilityWorkerCount(len(files), opts.ProbeOptions.CheckWorkers)
	if workers <= 1 {
		for i, file := range files {
			if ctx.Err() != nil {
				entries[i] = eligibilityEntry{File: file, Fatal: ctx.Err()}
				continue
			}
			entries[i] = classifier(ctx, mode, file, opts)
		}
		return eligibilityResultFromEntries(entries)
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				file := files[idx]
				if ctx.Err() != nil {
					entries[idx] = eligibilityEntry{File: file, Fatal: ctx.Err()}
					continue
				}
				// Workers write to their assigned index only. The result is
				// folded later in argv order so summaries, encode counters, and
				// probe JSON stay deterministic even though ffprobe runs in
				// parallel.
				entries[idx] = classifier(ctx, mode, file, opts)
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return eligibilityResultFromEntries(entries)
}

func eligibilityWorkerCount(files int, requested int) int {
	if files <= 1 {
		return 1
	}
	if requested < 1 {
		requested = 1
	}
	if requested > files {
		return files
	}
	return requested
}

func eligibilityResultFromEntries(entries []eligibilityEntry) eligibilityResult {
	var result eligibilityResult
	result.Entries = entries
	for _, entry := range entries {
		// Report the first fatal error by input order, not by whichever worker
		// finished first. This keeps repeated batch runs diagnosable.
		if result.Fatal == nil && entry.Fatal != nil {
			result.Fatal = entry.Fatal
		}
		if entry.Fatal != nil {
			continue
		}
		if entry.Skipped {
			result.Skipped = append(result.Skipped, skippedInput{File: entry.File, Reason: entry.SkipReason, Pattern: entry.Pattern})
			continue
		}
		result.Actionable = append(result.Actionable, eligibleInput{File: entry.File, Info: entry.Info})
	}
	return result
}

func classifyEligibilityInput(ctx context.Context, mode eligibilityMode, file string, opts EncodeOptions) eligibilityEntry {
	if pattern, ok := skipNameMatch(file, opts.ProbeOptions.SkipNames); ok {
		return eligibilityEntry{File: file, Skipped: true, SkipReason: eligibilitySkipName, Pattern: pattern}
	}
	// This is the only eligibility step that may spawn ffprobe. Keep all
	// filename-only checks above it so broad globs and explicit skip markers do
	// not spend work on paths we already know are no-ops.
	info, err := probeInputMediaContext(ctx, file)
	if err != nil {
		switch {
		case errors.Is(err, errNotVideoFile):
			return eligibilityEntry{File: file, Skipped: true, SkipReason: eligibilitySkipNotVideo}
		case errors.Is(err, errNoVideoStream):
			return eligibilityEntry{File: file, Skipped: true, SkipReason: eligibilitySkipNoVideo}
		default:
			return eligibilityEntry{File: file, Fatal: err}
		}
	}
	if mode == eligibilityEncode {
		if skip, ok, err := classifyEncodeMedia(file, info, opts); err != nil {
			return eligibilityEntry{File: file, Fatal: err}
		} else if ok {
			return eligibilityEntry{File: file, Info: info, Skipped: true, SkipReason: skip.Reason}
		}
	}
	return eligibilityEntry{File: file, Info: info}
}

func classifyEncodeMedia(file string, info MediaInfo, opts EncodeOptions) (skippedInput, bool, error) {
	if shouldSkipAlreadyEncoded(info, opts) {
		return skippedInput{File: file, Reason: eligibilitySkipAlreadyAV1}, true, nil
	}
	output := outputPathFor(file)
	if _, err := os.Stat(output); err == nil && !opts.Overwrite {
		return skippedInput{}, false, fmt.Errorf("output already exists: %s", displayPath(output))
	}
	return skippedInput{}, false, nil
}

func shouldSummarizeEligibility(files []string, json bool) bool {
	return len(files) > 1 && !json
}

func printEligibilityStart(files []string, json bool) {
	if shouldSummarizeEligibility(files, json) {
		fmt.Fprintf(os.Stderr, ">>> Checking %d inputs ...\n", len(files))
	}
}

func printEligibilitySummary(result eligibilityResult, total int, json bool) {
	if total <= 1 || json {
		return
	}
	fmt.Fprintf(os.Stderr, ">>> Eligible %d/%d inputs", len(result.Actionable), total)
	if len(result.Skipped) > 0 {
		fmt.Fprintf(os.Stderr, "; skipped %s", formatEligibilitySkipCounts(result.Skipped))
	}
	fmt.Fprintln(os.Stderr)
}

func formatEligibilitySkipCounts(skipped []skippedInput) string {
	if len(skipped) == 0 {
		return ""
	}
	counts := map[eligibilityReason]int{}
	for _, skip := range skipped {
		counts[skip.Reason]++
	}
	order := []eligibilityReason{eligibilitySkipNotVideo, eligibilitySkipNoVideo, eligibilitySkipAlreadyAV1, eligibilitySkipName}
	var parts []string
	for _, reason := range order {
		if count := counts[reason]; count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", count, reason))
		}
		delete(counts, reason)
	}
	var extra []string
	for reason := range counts {
		extra = append(extra, string(reason))
	}
	sort.Strings(extra)
	for _, reason := range extra {
		parts = append(parts, fmt.Sprintf("%d %s", counts[eligibilityReason(reason)], reason))
	}
	return strings.Join(parts, ", ")
}

func printSkippedInput(skip skippedInput) {
	switch skip.Reason {
	case eligibilitySkipName:
		fmt.Fprintln(os.Stderr, formatSkipNameMessage(skip.File, skip.Pattern))
	case eligibilitySkipNotVideo:
		fmt.Fprintf(os.Stderr, "%s: not a video file, skipping\n", displayPath(skip.File))
	case eligibilitySkipNoVideo:
		fmt.Fprintf(os.Stderr, "%s: no video stream found, skipping\n", displayPath(skip.File))
	case eligibilitySkipAlreadyAV1:
		fmt.Fprintf(os.Stderr, "%s: already .mkv with AV1 video, skipping\n", displayPath(skip.File))
	}
}

func probeSkippedResult(skip skippedInput, opts ProbeOptions) ProbeResult {
	msg := "skipped"
	switch skip.Reason {
	case eligibilitySkipName:
		msg = "name matched skip filter, skipped"
	case eligibilitySkipNotVideo:
		msg = "not a video file, skipped"
	case eligibilitySkipNoVideo:
		msg = "no video stream found, skipped"
	}
	return ProbeResult{File: skip.File, TargetVMAF: opts.TargetVMAF, FloorVMAF: opts.FloorVMAF, Error: msg}
}
