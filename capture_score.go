package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func capturedScoreArgs(sample SampleFile, distorted string, final *finalScoreWindow, filter, capturePath string) []string {
	args := []string{"-y", "-xerror"}
	if final != nil && !sample.FullPass {
		args = append(args, "-ss", terseFloat(final.Start.Seconds()), "-t", terseFloat(final.Duration.Seconds()))
	}
	args = append(args, "-i", distorted, "-ss", terseFloat(sample.Start.Seconds()))
	if !sample.FullPass {
		args = append(args, "-t", terseFloat((sample.Duration + 2*time.Second).Seconds()))
	}
	args = append(args, "-i", sample.SourcePath, "-filter_complex", captureFilter(filter, sample.Duration, sample.FullPass),
		"-map", "[quality]", "-fps_mode", "passthrough", "-an", "-sn", "-dn", "-f", "null", nullOutputPath())
	// Keep quality first: -progress reports the primary output frame count.
	return append(args, captureOutputArgs(capturePath)...)
}

func (s *crfSearch) scoreCapturedWindow(ctx context.Context, sample SampleFile, distorted string, final *finalScoreWindow, progress probeProgress, model VMAFModelInfo) (SampleDiagnostics, error) {
	started := time.Now()
	defer func() { s.timing.ScoreSeconds += time.Since(started).Seconds() }()
	if err := s.info.facts.check(s.info.Path); err != nil {
		return SampleDiagnostics{}, err
	}
	dir := s.options.TempDir
	if dir == "" {
		dir = filepath.Dir(sample.SourcePath)
	}
	logPath, err := reserveTempPath(dir, ".reencode-vmaf-*.json")
	if err != nil {
		return SampleDiagnostics{}, err
	}
	defer os.Remove(logPath)
	capturePath, err := reserveTempPath(dir, ".reencode-frames-*.crc")
	if err != nil {
		return SampleDiagnostics{}, err
	}
	defer os.Remove(capturePath)
	filter := model.filter(s.modelPath) + ":log_fmt=json:log_path=" + escapeFilterOption(logPath)
	role := "sample vmaf"
	if final != nil {
		role = "final quality"
	}
	res, err := executeVMAF(ctx, FFMpegRun{
		Role: role, File: s.info.Path, ScopeLabel: progress.Label, ScopeDone: progress.Done, ScopeTotal: progress.Total,
		Args: capturedScoreArgs(sample, distorted, final, filter, capturePath), StallTimeout: s.options.StallTimeout,
		ExpectedDuration: sample.Duration, ExpectedFrames: sample.Frames, Progress: s.options.progressDisplay(),
	})
	if err != nil {
		return SampleDiagnostics{}, err
	}
	s.options.progressDisplay().Phase("validating results")
	frames, err := loadFrameCapture(capturePath)
	if err != nil {
		return SampleDiagnostics{}, fmt.Errorf("invalid decoded timestamp capture: %w", err)
	}
	ref, err := timelineFromFrames(frames[1], 0, sample.Duration, sample.FullPass)
	if err != nil {
		return SampleDiagnostics{}, err
	}
	ref.Start += sample.Start
	dis, err := timelineFromFrames(frames[0], 0, 0, true)
	if err != nil {
		return SampleDiagnostics{}, err
	}
	if err := validateTimelines(ref, dis); err != nil {
		return SampleDiagnostics{}, err
	}
	if final != nil {
		dis.Start += final.Start
		if durationDifference(dis.Start-final.Offset, ref.Start) > 2*time.Millisecond {
			return SampleDiagnostics{}, fmt.Errorf("final sample begins at a different source frame")
		}
	}
	if sample.Timeline != nil {
		if err := validateTimelines(*sample.Timeline, ref); err != nil {
			return SampleDiagnostics{}, fmt.Errorf("source timeline changed: %w", err)
		}
		if durationDifference(sample.Timeline.Start, ref.Start) > 2*time.Millisecond {
			return SampleDiagnostics{}, fmt.Errorf("source window start changed")
		}
	}
	report, err := completedVMAFReport(res, logPath, len(ref.Times))
	if err != nil {
		return SampleDiagnostics{}, err
	}
	if err := s.info.facts.check(s.info.Path); err != nil {
		return SampleDiagnostics{}, err
	}
	s.initTimelines()
	s.timelines[sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}] = ref
	return diagnoseVMAF(report, ref), nil
}
