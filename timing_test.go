package main

import (
	"context"
	"fmt"
	"testing"
)

func TestProbeTimingIncludesFailedPreparation(t *testing.T) {
	old := checkScoringRuntime
	defer func() { checkScoringRuntime = old }()
	checkScoringRuntime = func(context.Context, ProbeOptions, VMAFModelInfo) (RuntimeVersions, error) {
		return RuntimeVersions{}, fmt.Errorf("unsupported runtime")
	}
	info := MediaInfo{Path: "input", preparation: ProbeTiming{MetadataSeconds: 2, HDRSeconds: 3, TotalSeconds: 5}}
	result, _, _, err := probeMediaFile(context.Background(), ProbeOptions{}, info, false)
	if err == nil || result.Timing.MetadataSeconds != 2 || result.Timing.HDRSeconds != 3 || result.Timing.RuntimeSeconds <= 0 || result.Timing.TotalSeconds < 5 {
		t.Fatal(result.Timing, err)
	}
}

func TestCompareTimingChargesPreparationOnce(t *testing.T) {
	old := compareProbe
	defer func() { compareProbe = old }()
	compareProbe = func(_ context.Context, _ ProbeOptions, info MediaInfo, _ bool) (ProbeResult, *probeSession, *probeCacheHandle, error) {
		if info.preparation != (ProbeTiming{}) {
			t.Fatal("shared preparation charged to row", info.preparation)
		}
		return ProbeResult{}, nil, nil, nil
	}
	info := MediaInfo{preparation: ProbeTiming{MetadataSeconds: 2, HDRSeconds: 3, TotalSeconds: 5}}
	result := compareInput(context.Background(), CompareOptions{Profiles: []string{"legacy", "v1"}, Tunes: []string{"psnr"}}, info)
	if result.Preparation != info.preparation || len(result.Results) != 2 {
		t.Fatal(result)
	}
}
