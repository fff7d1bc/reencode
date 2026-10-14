package main

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// Optional read-only check on representative compressed media, not a synthetic
// throughput claim. All sample encodes and reports belong to the test temp dir.
func TestMediaIntegrationCaptureBenchmark(t *testing.T) {
	requireMediaIntegration(t)
	path := os.Getenv("REENCODE_BENCH_MEDIA")
	if path == "" {
		t.Skip("set REENCODE_BENCH_MEDIA to a representative SDR source")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	info, err := probeInputMediaContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration < 250*time.Second {
		t.Skip("benchmark window requires at least 250 seconds")
	}
	opts := defaultProbeOptions()
	opts.Preset, opts.NoCache, opts.NoProgress, opts.TempDir = "11", true, true, t.TempDir()
	search := crfSearch{info: info, options: opts}
	defer search.cleanupEncodedSamples()
	sample := SampleFile{SourcePath: path, Start: 223 * time.Second, Duration: 20 * time.Second}
	prepared, err := search.prepareSample(ctx, sample)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := search.encodeSample(ctx, buildVideoArgs(info, opts.Preset, 32), prepared, 1, probeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := search.scoreSample(ctx, prepared, encoded, probeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	legacyTiming := search.timing
	search.timing = ProbeTiming{}
	search.options.captureBackend = capturePolicy
	captured, err := search.scoreSample(ctx, sample, encoded, probeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Frames != legacy.Frames || math.Abs(captured.Score-legacy.Score) > 0.00001 || math.Abs(captured.DurationSeconds-legacy.DurationSeconds) > 0.002 {
		t.Fatal("real-media backend mismatch", legacy, captured)
	}
	if err := info.facts.check(path); err != nil {
		t.Fatal("source changed", err)
	}
	t.Logf("%d frames, VMAF %.6f; legacy timestamp inspection %.3fs + scoring %.3fs = %.3fs; inline capture/scoring %.3fs (sample encoding shared, %.3fs)", captured.Frames, captured.Score, legacyTiming.InspectionSeconds, legacyTiming.ScoreSeconds, legacyTiming.InspectionSeconds+legacyTiming.ScoreSeconds, search.timing.ScoreSeconds, legacyTiming.EncodeSeconds)
}
