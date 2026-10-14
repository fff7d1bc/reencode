package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type finalScoreWindow struct {
	Origin, Start, Duration, Offset time.Duration
}

// Anchor the two playback clocks to their first decoded video frames. Audio
// priming can make the MKV format start negative without moving its video.
func firstVideoTime(ctx context.Context, info MediaInfo) (time.Duration, error) {
	out, err := frameProbeOutput(ctx, 30*time.Second, "-v", "error", "-of", "json", "-select_streams", "v:0", "-read_intervals", "%+#64", "-show_frames", "-show_entries", "frame=best_effort_timestamp_time:frame_side_data=", info.Path)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, fmt.Errorf("inspect video start for %s: %w", displayPath(info.Path), err)
	}
	var raw struct {
		Frames []struct {
			PTS string `json:"best_effort_timestamp_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return 0, err
	}
	if len(raw.Frames) == 0 {
		return 0, fmt.Errorf("missing first video frame")
	}
	pts, err := timestampDuration(raw.Frames[0].PTS)
	return pts - info.StartTime, err
}

func verificationWindow(sample SampleFile, output MediaInfo, offset time.Duration) finalScoreWindow {
	window := finalScoreWindow{Origin: output.StartTime, Offset: offset}
	if sample.FullPass {
		return window
	}
	// Use the verified reference frame endpoints with Matroska rounding slack,
	// not the requested seek boundary, which may lie exactly on a frame.
	window.Start = max(0, sample.Timeline.Start+offset-2*time.Millisecond)
	end := sample.Timeline.Start + sample.Timeline.Times[len(sample.Timeline.Times)-1] + offset + 2*time.Millisecond
	window.Duration = end - window.Start
	return window
}

func verificationTarget(opts ProbeOptions, probe *ProbeResult) float64 {
	if probe != nil && probe.Success && probe.EffectiveTarget > 0 {
		return probe.EffectiveTarget
	}
	return opts.TargetVMAF
}

func verifyFinalQuality(ctx context.Context, opts ProbeOptions, source MediaInfo, outputPath string, probe *ProbeResult) error {
	output, err := probeMediaContext(ctx, outputPath)
	if err != nil {
		return err
	}
	sourceStart, err := firstVideoTime(ctx, source)
	if err != nil {
		return err
	}
	outputStart, err := firstVideoTime(ctx, output)
	if err != nil {
		return err
	}
	session, err := newProbeSession(opts, source)
	if err != nil {
		return err
	}
	defer session.Close()
	if probe != nil && probe.Success && probe.SamplePlan.Count > 0 {
		plan := probe.SamplePlan
		plan.SampleDuration = time.Duration(plan.SampleSeconds * float64(time.Second))
		session.search.samples, err = createSamples(source, plan)
		if err != nil {
			return err
		}
	}
	score := func(ctx context.Context, sample SampleFile) (SampleDiagnostics, error) {
		prepared, err := session.search.prepareSample(ctx, sample)
		if err != nil {
			return SampleDiagnostics{}, err
		}
		window := verificationWindow(prepared, output, outputStart-sourceStart)
		return session.search.scoreWindow(ctx, prepared, outputPath, &window, probeProgress{Label: "verify final quality"})
	}
	attempt, err := verifyQualitySamples(ctx, &session.search, verificationTarget(opts, probe), score)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: final sampled quality passed, avg VMAF %.2f, min %.2f\n", displayPath(source.Path), attempt.Score, attempt.WorstSampleScore)
	if opts.Verbose {
		printSampleDiagnostics(attempt.Samples)
	}
	return nil
}

type finalSampleScorer func(context.Context, SampleFile) (SampleDiagnostics, error)

func verifyQualitySamples(ctx context.Context, search *crfSearch, target float64, score finalSampleScorer) (ProbeAttempt, error) {
	var attempt ProbeAttempt
	var scores []sampleScore
	for _, sample := range search.samples {
		if ctx.Err() != nil {
			return attempt, ctx.Err()
		}
		diagnostic, err := score(ctx, sample)
		if err != nil {
			return attempt, err
		}
		if diagnostic.Frames <= 0 || diagnostic.DurationSeconds <= 0 || !validVMAFScore(diagnostic.Score) {
			return attempt, fmt.Errorf("incomplete final quality sample")
		}
		attempt.Samples = append(attempt.Samples, diagnostic)
		attempt.sampleScores = append(attempt.sampleScores, diagnostic.Score)
		scores = append(scores, sampleScore{Score: diagnostic.Score, Duration: time.Duration(diagnostic.DurationSeconds * float64(time.Second))})
	}
	if len(scores) == 0 {
		return attempt, fmt.Errorf("no final quality samples")
	}
	summary := summarizeScores(scores)
	attempt.Score, attempt.WorstSampleScore = summary.Mean, summary.Worst
	// Never reuse probe outlier acceptance: confirm using neighboring windows
	// from the actual final output, with the same floor/tolerance policy.
	if !search.options.NoOutlierCheck && attempt.Score >= target && attempt.WorstSampleScore < search.options.FloorVMAF {
		if idx, ok := search.singleBorderlineOutlier(attempt); ok {
			neighbors := search.outlierNeighborSamples(search.samples[idx])
			attempt.OutlierChecked = len(neighbors) > 0
			attempt.OutlierScore = attempt.sampleScores[idx]
			for _, neighbor := range neighbors {
				if ctx.Err() != nil {
					return attempt, ctx.Err()
				}
				diagnostic, err := score(ctx, neighbor)
				if err != nil {
					return attempt, err
				}
				if diagnostic.Frames <= 0 || diagnostic.DurationSeconds <= 0 || !validVMAFScore(diagnostic.Score) {
					return attempt, fmt.Errorf("incomplete final quality neighbor")
				}
				attempt.OutlierNeighborScores = append(attempt.OutlierNeighborScores, diagnostic.Score)
			}
			attempt.OutlierAccepted = allScoresAtLeast(attempt.OutlierNeighborScores, search.options.FloorVMAF)
		}
	}
	if ctx.Err() != nil {
		return attempt, ctx.Err()
	}
	if !search.qualityOK(attempt, target) {
		return attempt, fmt.Errorf("sampled VMAF %.2f (target %.2f), worst %.2f (floor %.2f)", attempt.Score, target, attempt.WorstSampleScore, search.options.FloorVMAF)
	}
	return attempt, nil
}
