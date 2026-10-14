package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapturedScoreCleanupOnFailure(t *testing.T) {
	old := executeVMAF
	defer func() { executeVMAF = old }()
	for _, mode := range []string{"cancel", "missing", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			executeVMAF = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				if mode == "cancel" {
					return FFMpegResult{}, context.Canceled
				}
				if mode == "malformed" {
					if err := os.WriteFile(run.Args[len(run.Args)-1], []byte("0, broken"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return FFMpegResult{LastFrame: 1}, nil
			}
			s := crfSearch{info: MediaInfo{Path: "source"}, options: ProbeOptions{KeepTemp: true, TempDir: dir}}
			_, err := s.scoreCapturedWindow(context.Background(), SampleFile{FullPass: true}, "encoded", nil, probeProgress{}, resolveVMAFModel(ProbeOptions{VMAFProfile: "legacy"}, MediaInfo{}))
			if err == nil {
				t.Fatal("invalid capture accepted")
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatal("sidecar leak", entries, err)
			}
		})
	}
}

func TestMediaIntegrationCapturedScoreParity(t *testing.T) {
	requireMediaIntegration(t)
	for _, vfr := range []bool{false, true} {
		t.Run(fmt.Sprint(vfr), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.mov")
			args := []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24000/1001", "-t", "2"}
			if vfr {
				args = append(args, "-vf", "select=not(mod(n\\,3))", "-fps_mode", "vfr")
			}
			mediaFixture(t, append(args, "-c:v", "mpeg4", "-q:v", "2", "-output_ts_offset", "5", path)...)
			info, err := probeInputMediaContext(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			opts := defaultProbeOptions()
			opts.Preset, opts.NoCache, opts.NoProgress, opts.captureBackend = "11", true, true, capturePolicy
			search := crfSearch{info: info, options: opts}
			defer search.cleanupEncodedSamples()
			for _, sample := range []SampleFile{
				{SourcePath: path, Start: 123 * time.Millisecond, Duration: 700 * time.Millisecond},
				{SourcePath: path, Start: 1500 * time.Millisecond, Duration: time.Second},
				{SourcePath: path, FullPass: true, Duration: info.Duration},
			} {
				old := inspectionCommandContext
				inspectionCommandContext = func(context.Context, string, ...string) *exec.Cmd {
					t.Fatal("capture path performed separate frame inspection")
					return nil
				}
				prepared, err := search.prepareProbeSample(context.Background(), sample)
				if err != nil || prepared.Timeline != nil {
					t.Fatal("unexpected pre-scan", prepared, err)
				}
				encoded, _, err := search.encodeSample(context.Background(), buildVideoArgs(info, opts.Preset, 32), prepared, 1, probeProgress{})
				if err != nil {
					inspectionCommandContext = old
					t.Fatal(err)
				}
				captured, err := search.scoreSample(context.Background(), prepared, encoded, probeProgress{})
				inspectionCommandContext = old
				if err != nil {
					t.Fatal(err)
				}
				search.options.captureBackend = inspectionPolicy
				// Compare against fresh legacy inspection, not shared captured facts.
				ref, err := inspectTimeline(context.Background(), path, info.StartTime, sample.Start, sample.Duration, sample.FullPass)
				if err != nil {
					t.Fatal(err)
				}
				prepared, err = search.applyTimeline(sample, ref)
				if err != nil {
					t.Fatal(err)
				}
				legacy, err := search.scoreSample(context.Background(), prepared, encoded, probeProgress{})
				if err != nil || legacy.Frames != captured.Frames || math.Abs(legacy.Score-captured.Score) > 0.00001 || math.Abs(legacy.DurationSeconds-captured.DurationSeconds) > 0.002 {
					t.Fatal("backend mismatch", legacy, captured, err)
				}
				search.options.captureBackend = capturePolicy
			}
			search.cleanupEncodedSamples()
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					t.Fatal("scratch leak", entry.Name())
				}
			}
		})
	}
}
