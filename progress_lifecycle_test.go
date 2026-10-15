package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestProgressActivityHandoffs(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		p := &ProgressDisplay{out: &buf, live: true}
		fileDone := p.StartActivity("preparing probe", "/private/movie.mkv", 0)
		defer fileDone()
		assertActivity := func(role string) {
			t.Helper()
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.renderedLines == 0 || !p.state.Activity || p.state.Role != role || p.state.File != "/private/movie.mkv" {
				t.Fatalf("missing activity %q: %+v", role, p.state)
			}
		}
		assertActivity("preparing probe")
		p.Start(ProgressState{Role: "sample encode", File: "/private/movie.mkv", Frame: 12})
		before := buf.String()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if buf.String() != before || p.state.Activity || p.state.Frame != 12 || p.state.Elapsed != 0 {
			t.Fatal("background activity overwrote measured ffmpeg progress")
		}
		p.Finish()
		assertActivity("preparing probe")
		qualityDone := p.StartActivity("preparing quality check", "/private/movie.mkv", 0)
		defer qualityDone()
		assertActivity("preparing quality check")
		p.Start(ProgressState{Role: "sample vmaf", File: "/private/movie.mkv", Frame: 24})
		p.Finish()
		assertActivity("preparing quality check")
		p.Phase("validating results")
		time.Sleep(2 * time.Second)
		synctest.Wait()
		assertActivity("validating results")
		if p.state.Elapsed < time.Second {
			t.Fatal("result validation did not show elapsed activity")
		}
		p.Printf("a permanent diagnostic\n")
		assertActivity("validating results")
		if !strings.Contains(buf.String(), "a permanent diagnostic\n\x1b[2K    validating results") {
			t.Fatal("diagnostic was not followed by a restored status line")
		}
		var stdout bytes.Buffer
		p.PrintTo(&stdout, "ffmpeg dry-run command")
		if stdout.String() != "ffmpeg dry-run command\n" {
			t.Fatal("stdout mixed with interactive output", stdout.String())
		}
		assertActivity("validating results")
		qualityDone()
		assertActivity("preparing probe")
		fileDone()
		fileDone()
		if p.renderedLines != 0 || len(p.activities) != 0 || p.activity != nil {
			t.Fatal("file completion left a live scope")
		}
		before = buf.String()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if buf.String() != before || strings.Contains(before, "/private") {
			t.Fatal("late redraw or full-path leak")
		}
	})
}

func TestProgressActivityOutOfOrderCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		p := &ProgressDisplay{out: &buf, live: true}
		outer := p.StartActivity("outer", "movie.mkv", 0)
		inner := p.StartActivity("inner", "movie.mkv", 0)
		defer outer()
		defer inner()
		outer()
		if p.state.Role != "inner" || p.renderedLines == 0 {
			t.Fatal("outer cleanup cleared the current phase")
		}
		p.Start(ProgressState{Role: "encode", File: "movie.mkv"})
		inner()
		if p.state.Role != "encode" || p.renderedLines == 0 {
			t.Fatal("activity cleanup cleared the current subprocess")
		}
		p.Finish()
		if p.renderedLines != 0 || len(p.activities) != 0 {
			t.Fatal("subprocess resurrected a finished activity")
		}
	})
}

func TestFFmpegRestoresActivityOnEveryExit(t *testing.T) {
	for _, outcome := range []string{"score-exit", "failure-exit", "cancel", "stall", "start-error"} {
		t.Run(outcome, func(t *testing.T) {
			mode := outcome
			if outcome == "cancel" || outcome == "stall" {
				mode = "hang"
			}
			restore := useFFmpegHelperProcess(t, mode)
			defer restore()
			if outcome == "start-error" {
				missing := filepath.Join(t.TempDir(), "missing-ffmpeg")
				ffmpegCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
					return exec.CommandContext(ctx, missing)
				}
			}
			var buf bytes.Buffer
			p := &ProgressDisplay{out: &buf, live: true}
			stop := p.StartActivity("preparing quality check", "movie.mkv", 0)
			defer stop()
			ctx := context.Background()
			if outcome == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			run := FFMpegRun{Role: "sample encode", File: "movie.mkv", Progress: p}
			if outcome == "stall" {
				run.StallTimeout = 50 * time.Millisecond
			}
			_, err := runFFmpeg(ctx, run)
			if (err == nil) != (outcome == "score-exit") {
				t.Fatalf("unexpected outcome: %v", err)
			}
			p.mu.Lock()
			visible := p.state.Activity && p.state.Role == "preparing quality check" && p.renderedLines > 0
			p.mu.Unlock()
			if !visible {
				t.Fatal("subprocess exit left a blank status")
			}
			stop()
			if p.renderedLines != 0 || len(p.activities) != 0 {
				t.Fatal("status survived file completion")
			}
			if !strings.Contains(buf.String(), "sample encode") {
				t.Fatal("subprocess had no status, including during startup")
			}
		})
	}
}

func TestOutputInspectionActivityAndQuietModes(t *testing.T) {
	old := inspectFinalMedia
	t.Cleanup(func() { inspectFinalMedia = old })
	for _, mode := range []string{"interactive", "json", "no-progress"} {
		for _, outcome := range []string{"success", "error", "canceled"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				var buf bytes.Buffer
				p := &ProgressDisplay{out: &buf, live: true}
				opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
				opts.ProbeOptions.Progress, opts.ProbeOptions.JSON, opts.ProbeOptions.NoProgress = p, mode == "json", mode == "no-progress"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				inspectFinalMedia = func(context.Context, string) (MediaInfo, error) {
					p.mu.Lock()
					visible := p.renderedLines > 0 && p.state.Activity && p.state.Role == "validating output"
					p.mu.Unlock()
					if visible != (mode == "interactive") {
						t.Fatal("wrong visibility during output ffprobe")
					}
					if outcome == "canceled" {
						cancel()
						return MediaInfo{}, ctx.Err()
					}
					if outcome == "error" {
						return MediaInfo{}, errors.New("inspection failed")
					}
					return validationOutputInfo(), nil
				}
				source := validationSourceInfo()
				source.Path = "/private/movie.mkv"
				err := validateOutput(ctx, "/private/.temporary.mkv", source, opts)
				if (err == nil) != (outcome == "success") {
					t.Fatalf("unexpected outcome: %v", err)
				}
				if p.renderedLines != 0 || len(p.activities) != 0 || mode != "interactive" && buf.Len() != 0 {
					t.Fatal("inspection leaked a status or printed in quiet mode")
				}
				if strings.Contains(buf.String(), "/private") || strings.Contains(buf.String(), ".temporary") {
					t.Fatal("validation status should identify the input basename")
				}
			})
		}
	}
}

func TestScoringActivityThroughValidationFailure(t *testing.T) {
	old := executeVMAF
	t.Cleanup(func() { executeVMAF = old })
	for _, mode := range []string{"interactive", "json", "no-progress"} {
		t.Run(mode, func(t *testing.T) {
			var buf bytes.Buffer
			p := &ProgressDisplay{out: &buf, live: true}
			opts := defaultProbeOptions()
			opts.Progress, opts.JSON, opts.NoProgress = p, mode == "json", mode == "no-progress"
			opts.TempDir, opts.VMAFProfile = t.TempDir(), "legacy"
			opts.runtime.LibVMAF, opts.captureBackend = "test", capturePolicy
			executeVMAF = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				if (run.Progress != nil) != (mode == "interactive") {
					t.Fatal("scoring ignored quiet mode")
				}
				run.Progress.Start(ProgressState{Role: run.Role, File: run.File})
				run.Progress.Finish()
				p.mu.Lock()
				visible := p.renderedLines > 0 && p.state.Activity && p.state.Role == "preparing quality check"
				p.mu.Unlock()
				if visible != (mode == "interactive") {
					t.Fatal("scoring exit left a blank validation interval")
				}
				return FFMpegResult{}, os.WriteFile(run.Args[len(run.Args)-1], []byte("malformed capture"), 0o600)
			}
			s := crfSearch{options: opts, info: MediaInfo{Path: "/private/movie.mkv"}}
			_, err := s.scoreSample(context.Background(), SampleFile{FullPass: true}, "sample.mkv", probeProgress{})
			if err == nil {
				t.Fatal("malformed result accepted")
			}
			if p.renderedLines != 0 || len(p.activities) != 0 || mode != "interactive" && buf.Len() != 0 {
				t.Fatal("scoring leaked a status or printed in quiet mode")
			}
			if mode == "interactive" && !strings.Contains(buf.String(), "validating results") {
				t.Fatal("post-VMAF validation was silent")
			}
		})
	}
}

func TestMediaIntegrationContinuousStatus(t *testing.T) {
	requireMediaIntegration(t)
	path := filepath.Join(t.TempDir(), "source.mkv")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24", "-t", "3", "-c:v", "ffv1", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p := &ProgressDisplay{out: &buf, live: true}
	opts := defaultProbeOptions()
	opts.Progress, opts.Preset, opts.NoCache = p, "11", true
	opts.InputChecks = "per-file"
	opts.TargetVMAF, opts.FloorVMAF, opts.MaxEncodedPercent = 80, 75, 100
	opts.Samples, opts.SampleDuration, opts.Verbose = 1, 500*time.Millisecond, true
	old := inspectionCommandContext
	t.Cleanup(func() { inspectionCommandContext = old })
	inspectionCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		p.mu.Lock()
		visible := p.renderedLines > 0 && p.state.Activity
		p.mu.Unlock()
		if !visible {
			t.Error("decoded ffprobe inspection has no activity status")
		}
		return old(ctx, name, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if code := runProbeCommand(ctx, opts, []string{path}); code != 0 {
		t.Fatalf("probe failed: %d", code)
	}
	if p.renderedLines != 0 || len(p.activities) != 0 {
		t.Fatal("probe leaked live status into the next file")
	}
	// Verify at a conservative fixed CRF: sample/final boundary differences
	// should not make this display-lifecycle test depend on a marginal score.
	if code := runEncodeCommand(ctx, EncodeOptions{ProbeOptions: opts, CRFSet: true, CRF: 20, KeepSource: true, VerifyQuality: true}, []string{path}); code != 0 {
		t.Fatalf("encode failed: %d", code)
	}
	if p.renderedLines != 0 || len(p.activities) != 0 || p.activity != nil {
		t.Fatal("encode leaked live status")
	}
	for _, role := range []string{"checking inputs", "input HDR checks", "checking probe cache", "preparing sample", "sample encode", "preparing quality check", "sample vmaf", "validating results", "saving sample result", "final encode", "validating output", "reading output metadata", "checking source video start", "checking output video start", "final quality", "publishing output"} {
		if !strings.Contains(buf.String(), role) {
			t.Errorf("missing phase %q", role)
		}
	}
	if strings.Contains(buf.String(), filepath.Dir(path)) {
		t.Fatal("status leaked an input directory")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("test source changed")
	}
}
