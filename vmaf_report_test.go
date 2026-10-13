package main

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reportJSON(scores ...float64) string {
	var frames []map[string]any
	var reciprocal float64
	for i, score := range scores {
		frames = append(frames, map[string]any{"frameNum": i, "metrics": map[string]float64{"vmaf": score}})
		reciprocal += 1 / (score + 1)
	}
	mean := float64(len(scores))/reciprocal - 1
	data, _ := json.Marshal(map[string]any{"version": "3.2.1", "frames": frames, "pooled_metrics": map[string]any{"vmaf": map[string]float64{"harmonic_mean": mean}}})
	return string(data)
}

func TestStructuredVMAFReport(t *testing.T) {
	valid := reportJSON(95, 96, 97)
	report, err := readVMAFReport(strings.NewReader(valid), "vmaf", 3)
	if err != nil || len(report.Frames) != 3 || report.Version != "3.2.1" {
		t.Fatalf("report=%+v, %v", report, err)
	}
	for _, input := range []string{"", `{}`, valid + "{}", strings.Replace(valid, `"frameNum":1`, `"frameNum":0`, 1), strings.Replace(valid, `"vmaf":96`, `"vmaf":null`, 1), strings.Replace(valid, `"vmaf":96`, `"vmaf":-2`, 1), strings.Replace(valid, `"vmaf":96`, `"vmaf":101`, 1), strings.Replace(valid, `"harmonic_mean":`, `"unrelated":`, 1), strings.Replace(valid, `"vmaf":96`, `"vmaf":1e999`, 1), strings.Replace(valid, `"version":"3.2.1"`, `"version":""`, 1), strings.Replace(valid, `"frameNum":1,`, ``, 1)} {
		if _, err := readVMAFReport(strings.NewReader(input), "vmaf", 3); err == nil {
			t.Fatalf("accepted invalid report: %s", input)
		}
	}
	for _, count := range []int{0, 2, 4} {
		if _, err := readVMAFReport(strings.NewReader(valid), "vmaf", count); err == nil {
			t.Fatalf("accepted wrong count %d", count)
		}
	}
	if _, err := readVMAFReport(strings.NewReader(valid), "other-model", 3); err == nil {
		t.Fatal("accepted unexpected model")
	}
	if _, err := readVMAFReport(strings.NewReader(reportJSON(0, 100)), "vmaf", 2); err != nil {
		t.Fatalf("zero score rejected: %v", err)
	}
}

func TestVMAFSuccessWithoutReportIsFailure(t *testing.T) {
	original := executeVMAF
	t.Cleanup(func() { executeVMAF = original })
	executeVMAF = func(context.Context, FFMpegRun) (FFMpegResult, error) { return FFMpegResult{LastFrame: 2}, nil }
	if _, err := runVMAFReport(context.Background(), FFMpegRun{}, filepath.Join(t.TempDir(), "missing.json"), 2); err == nil {
		t.Fatal("trusted zero exit code without scores")
	}
}

func TestVMAFDiagnosticsUseFrameDurations(t *testing.T) {
	report, err := readVMAFReport(strings.NewReader(reportJSON(100, 50, 100)), "vmaf", 3)
	if err != nil {
		t.Fatal(err)
	}
	diag := diagnoseVMAF(report, frameTimeline{Start: 10 * time.Second, Times: []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond}, Duration: 2 * time.Second})
	if diag.FrameP05 != 50 || math.Abs(diag.WorstSecondScore-50) > 1e-6 || diag.WorstSecondStart != 10.5 || diag.WorstSecondDuration != 1 {
		t.Fatalf("diagnostics=%+v", diag)
	}
	short := diagnoseVMAF(vmafReport{Score: 0, Frames: []float64{0}}, frameTimeline{Times: []time.Duration{0}, Duration: 40 * time.Millisecond})
	if short.WorstSecondScore != 0 || short.WorstSecondDuration != 0.04 {
		t.Fatalf("short diagnostic=%+v", short)
	}
}

func TestFilterOptionEscapesBothLayers(t *testing.T) {
	for _, value := range []string{`C:\samples\score.json`, `a'b:c,[d];e.json`, " a b "} {
		got := escapeFilterOption(value)
		if got == value || !strings.Contains(got, `\\`) {
			t.Fatalf("not escaped twice: %q => %q", value, got)
		}
	}
}
