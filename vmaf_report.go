package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"
)

type SampleDiagnostics struct {
	StartSeconds        float64 `json:"start_seconds"`
	DurationSeconds     float64 `json:"duration_seconds"`
	Frames              int     `json:"frames"`
	Score               float64 `json:"score"`
	FrameP05            float64 `json:"frame_p05"`
	WorstSecondScore    float64 `json:"worst_second_score"`
	WorstSecondStart    float64 `json:"worst_second_start"`
	WorstSecondDuration float64 `json:"worst_second_duration"`
}

type vmafReport struct {
	Version string
	Score   float64
	Frames  []float64
}

var executeVMAF = runFFmpeg

func readVMAFReport(r io.Reader, metric string, expected int) (vmafReport, error) {
	var report vmafReport
	dec := json.NewDecoder(r)
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return report, fmt.Errorf("missing VMAF JSON object")
	}
	var pooled *float64
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return report, err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return report, fmt.Errorf("invalid or duplicate VMAF JSON field")
		}
		seen[key] = true
		switch key {
		case "version":
			if err := dec.Decode(&report.Version); err != nil {
				return report, err
			}
		case "frames":
			if token, err := dec.Token(); err != nil || token != json.Delim('[') {
				return report, fmt.Errorf("missing VMAF frames array")
			}
			for dec.More() {
				var frame struct {
					Number  *int                `json:"frameNum"`
					Metrics map[string]*float64 `json:"metrics"`
				}
				if err := dec.Decode(&frame); err != nil {
					return report, err
				}
				score := frame.Metrics[metric]
				if frame.Number == nil || *frame.Number != len(report.Frames) || len(report.Frames) >= expected || score == nil || !validVMAFScore(*score) {
					return report, fmt.Errorf("missing, invalid, or unexpected VMAF frame %d for %s", len(report.Frames), metric)
				}
				report.Frames = append(report.Frames, *score)
			}
			if _, err := dec.Token(); err != nil {
				return report, err
			}
		case "pooled_metrics":
			var metrics map[string]struct {
				Harmonic *float64 `json:"harmonic_mean"`
			}
			if err := dec.Decode(&metrics); err != nil {
				return report, err
			}
			pooled = metrics[metric].Harmonic
		default:
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return report, err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return report, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return report, fmt.Errorf("trailing data after VMAF JSON")
	}
	if expected <= 0 || len(report.Frames) != expected || pooled == nil || !validVMAFScore(*pooled) || report.Version == "" {
		return report, fmt.Errorf("incomplete VMAF report: scored %d of %d frames", len(report.Frames), expected)
	}
	var reciprocal float64
	for _, score := range report.Frames {
		reciprocal += 1 / (score + 1)
	}
	if math.Abs(float64(expected)/reciprocal-1-*pooled) > 0.0001 {
		return report, fmt.Errorf("VMAF pooled score disagrees with frame scores")
	}
	report.Score = *pooled
	return report, nil
}

func validVMAFScore(score float64) bool {
	// The supported models clip to 0..100. Pooling can overshoot the upper
	// endpoint by floating-point roundoff (e.g. 100.00000000000001).
	return !math.IsNaN(score) && !math.IsInf(score, 0) && score >= 0 && score <= 100.000001
}

func runVMAFReport(ctx context.Context, run FFMpegRun, path string, frames int) (vmafReport, error) {
	res, err := executeVMAF(ctx, run)
	if err != nil {
		return vmafReport{}, err
	}
	if res.LastFrame != int64(frames) {
		return vmafReport{}, fmt.Errorf("VMAF frame coverage mismatch: scored %d, expected %d", res.LastFrame, frames)
	}
	f, err := os.Open(path)
	if err != nil {
		if pathErr, ok := err.(*os.PathError); ok {
			err = pathErr.Err
		}
		return vmafReport{}, fmt.Errorf("read VMAF report: %w", err)
	}
	defer f.Close()
	report, err := readVMAFReport(f, "vmaf", frames)
	if err != nil {
		return report, fmt.Errorf("invalid VMAF report: %w", err)
	}
	return report, nil
}

func diagnoseVMAF(report vmafReport, timeline frameTimeline) SampleDiagnostics {
	sorted := append([]float64(nil), report.Frames...)
	sort.Float64s(sorted)
	worst, start, duration := worstQualityWindow(report.Frames, timeline)
	return SampleDiagnostics{
		StartSeconds: timeline.Start.Seconds(), DurationSeconds: timeline.Duration.Seconds(), Frames: len(sorted), Score: report.Score,
		FrameP05:         sorted[max(0, int(math.Ceil(float64(len(sorted))*0.05))-1)],
		WorstSecondScore: worst, WorstSecondStart: (timeline.Start + start).Seconds(), WorstSecondDuration: duration.Seconds(),
	}
}

func worstQualityWindow(scores []float64, timeline frameTimeline) (float64, time.Duration, time.Duration) {
	window := min(time.Second, timeline.Duration)
	ends := append(append([]time.Duration(nil), timeline.Times...), timeline.Duration)
	prefix := make([]float64, len(ends))
	for i, score := range scores {
		prefix[i+1] = prefix[i] + (ends[i+1]-ends[i]).Seconds()/(score+1)
	}
	integral := func(at time.Duration) float64 {
		i := sort.Search(len(ends), func(i int) bool { return ends[i] > at }) - 1
		if i >= len(scores) {
			return prefix[len(scores)]
		}
		return prefix[i] + (at-ends[i]).Seconds()/(scores[i]+1)
	}
	// The window integral changes slope only when either edge crosses a frame
	// boundary. Check both sets of boundaries, using actual VFR durations.
	worst, position := math.Inf(1), time.Duration(0)
	for _, edge := range ends {
		for _, start := range []time.Duration{edge, edge - window} {
			if start < 0 || start+window > timeline.Duration {
				continue
			}
			score := window.Seconds()/(integral(start+window)-integral(start)) - 1
			if score < worst {
				worst, position = score, start
			}
		}
	}
	return worst, position, window
}

// Filtergraph syntax and AVOption values each unescape once. These are not
// shell escapes: ffmpeg receives the resulting string as one argv element.
func escapeFilterOption(value string) string {
	return escapeFFmpegToken(escapeFFmpegToken(value, ":"), "[],;")
}

func escapeFFmpegToken(value, separators string) string {
	var out strings.Builder
	for _, ch := range value {
		if strings.ContainsRune("\\' \t\r\n"+separators, ch) {
			out.WriteByte('\\')
		}
		out.WriteRune(ch)
	}
	return out.String()
}
