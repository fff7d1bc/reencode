package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func timelineJSON(points ...string) []byte {
	frames := make([]map[string]string, len(points))
	for i, pts := range points {
		frames[i] = map[string]string{"best_effort_timestamp_time": pts, "duration_time": "0.04"}
	}
	data, _ := json.Marshal(map[string]any{"frames": frames})
	return data
}

func TestTimelineWindowCoverage(t *testing.T) {
	got, err := parseTimeline(timelineJSON("10", "10.04", "10.08", "10.20", "10.32", "10.44"), 10*time.Second, 50*time.Millisecond, 300*time.Millisecond, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != 80*time.Millisecond || got.Duration != 270*time.Millisecond || len(got.Times) != 3 || got.Times[2] != 240*time.Millisecond {
		t.Fatalf("timeline=%+v", got)
	}
	full, err := parseTimeline(timelineJSON("10", "10.04", "10.08"), 10*time.Second, 0, time.Millisecond, true)
	if err != nil || len(full.Times) != 3 || full.Duration != 120*time.Millisecond {
		t.Fatalf("EOF timeline=%+v, %v", full, err)
	}
}

func TestTimelineRejectsBadFrames(t *testing.T) {
	for _, data := range [][]byte{timelineJSON(), timelineJSON("NaN"), timelineJSON("Inf"), timelineJSON("N/A"), timelineJSON("0", "0"), timelineJSON("1", "0"), []byte(`{`)} {
		if _, err := parseTimeline(data, 0, 0, 0, true); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
	ref := frameTimeline{Times: []time.Duration{0, 41667 * time.Microsecond, 83333 * time.Microsecond}}
	if err := validateTimelines(ref, frameTimeline{Times: []time.Duration{0, 42 * time.Millisecond, 83 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	for _, times := range [][]time.Duration{{0, 42 * time.Millisecond}, {0, 42 * time.Millisecond, 125 * time.Millisecond}} {
		if err := validateTimelines(ref, frameTimeline{Times: times}); err == nil {
			t.Fatal("accepted missing or shifted frames")
		}
	}
}

func TestFullPassHasNoEstimatedCutoff(t *testing.T) {
	sample := SampleFile{SourcePath: "source.mkv", FullPass: true, Frames: 1, Duration: time.Second}
	for _, args := range [][]string{sampleEncodeArgs(VideoArgs{}, sample, "sample.mkv"), sampleScoreArgs(MediaInfo{}, sample, "sample.mkv")} {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-frames:v") || strings.Contains(joined, "-t ") {
			t.Fatalf("full-pass cutoff: %s", joined)
		}
	}
}

func TestSharedVideoArgsPreserveTimestamps(t *testing.T) {
	for _, codecOption := range []string{"-c:v", "-c:v:0"} {
		args := strings.Join(buildVideoArgs(MediaInfo{}, "4", 32).ffmpegArgs(codecOption), " ")
		if !strings.Contains(args, "-enc_time_base:v:0 demux") || !strings.Contains(args, "-fps_mode passthrough") {
			t.Fatalf("missing shared timestamp settings: %s", args)
		}
	}
}
