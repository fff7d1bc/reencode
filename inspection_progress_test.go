package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestInspectionActivityDisplayLifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		p := &ProgressDisplay{out: &buf, live: true}
		stop := p.StartActivity("source timestamps", "/private/video/movie.mkv", 0)
		defer stop()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		p.mu.Lock()
		state, output := p.state, buf.String()
		p.mu.Unlock()
		if state.Elapsed < time.Second || !strings.Contains(output, "elapsed 0:01") || !strings.Contains(output, "movie.mkv") {
			t.Fatalf("no elapsed activity: %+v, %q", state, output)
		}
		if strings.Contains(output, "/private") || strings.Contains(output, "f=") || strings.Contains(output, "%") {
			t.Fatalf("inspection claims measured progress or leaks paths: %q", output)
		}
		stop()
		stop() // Cleanup is safe even if an error path already stopped the phase.
		if p.renderedLines != 0 {
			t.Fatal("inspection display was not cleared")
		}
		p.Start(ProgressState{Role: "encode", File: "next.mkv"})
		before := buf.String()
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if buf.String() != before || p.state.Elapsed != 0 || p.state.Activity {
			t.Fatal("inspection ticker changed the next phase")
		}
		p.Finish()
	})
}

func TestBatchInspectionProgress(t *testing.T) {
	for _, workers := range []int{1, 4, 32} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			var buf bytes.Buffer
			p := &ProgressDisplay{out: &buf, live: true}
			opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
			opts.ProbeOptions.Progress, opts.ProbeOptions.CheckWorkers = p, workers
			files := make([]string, 3000)
			for i := range files {
				files[i] = fmt.Sprintf("file%d.mkv", i)
			}
			result := collectEligibleInputsWithClassifier(context.Background(), eligibilityEncode, files, opts,
				func(_ context.Context, _ eligibilityMode, file string, _ EncodeOptions) eligibilityEntry {
					return eligibilityEntry{File: file, Skipped: true, SkipReason: eligibilitySkipAlreadyAV1}
				})
			if p.state.ScopeDone != 3000 || p.state.Skipped != 3000 || p.renderedLines != 0 {
				t.Fatalf("incorrect final progress: %+v", p)
			}
			if !strings.Contains(buf.String(), "checking inputs 3000/3000 (3000 skipped)") {
				t.Fatal("final counts were not rendered")
			}
			for i, entry := range result.Entries {
				if entry.File != files[i] {
					t.Fatal("progress changed input ordering")
				}
			}
		})
	}
}

func TestTimelineActivityCleanupAndQuietModes(t *testing.T) {
	old := inspectionCommandContext
	t.Cleanup(func() { inspectionCommandContext = old })
	for _, mode := range []string{"interactive", "json", "no-progress"} {
		for _, outcome := range []string{"success", "error", "canceled"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				var buf bytes.Buffer
				p := &ProgressDisplay{out: &buf, live: true}
				opts := defaultProbeOptions()
				opts.Progress, opts.JSON, opts.NoProgress = p, mode == "json", mode == "no-progress"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				inspectionCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
					p.mu.Lock()
					active := p.renderedLines > 0 && p.state.Activity
					p.mu.Unlock()
					if active != (mode == "interactive") {
						t.Error("incorrect visibility before decoding starts")
					}
					if outcome == "canceled" {
						cancel()
					}
					data := string(timelineJSON("0", "0.04", "0.08"))
					if outcome == "error" {
						data = "invalid JSON"
					}
					return inspectionHelperCommand(ctx, data)
				}
				s := crfSearch{options: opts, info: MediaInfo{Path: "/private/source.mkv"}}
				for _, role := range []string{"source timestamps", "sample timestamps", "final timestamps"} {
					_, err := s.inspectTimeline(ctx, "sample.mkv", 0, 0, 0, true, role)
					if (err == nil) != (outcome == "success") || outcome == "canceled" && !errors.Is(err, context.Canceled) {
						t.Fatalf("unexpected error: %v", err)
					}
					if p.renderedLines != 0 {
						t.Fatal("inspection display leaked on return")
					}
					if mode == "interactive" && !strings.Contains(buf.String(), role) {
						t.Fatalf("missing inspection role %q", role)
					}
				}
				collectEligibleInputsWithClassifier(ctx, eligibilityProbe, []string{"source.mkv"}, EncodeOptions{ProbeOptions: opts},
					func(context.Context, eligibilityMode, string, EncodeOptions) eligibilityEntry {
						return eligibilityEntry{Fatal: errors.New("inspection failed")}
					})
				if p.renderedLines != 0 || mode != "interactive" && buf.Len() != 0 {
					t.Fatalf("batch display leaked or quiet mode printed: %q", buf.String())
				}
			})
		}
	}
}

func TestRuntimeChecksUseProgressPolicy(t *testing.T) {
	oldTune, oldVMAF := executeTuneCheck, executeVMAF
	oldTuneCache, oldScoringCache := encoderTuneCache.versions, scoringRuntimeCache.versions
	t.Cleanup(func() {
		executeTuneCheck, executeVMAF = oldTune, oldVMAF
		encoderTuneCache.versions, scoringRuntimeCache.versions = oldTuneCache, oldScoringCache
	})
	const banner = "SVT-AV1 Encoder Lib v4.2.0\nSvt[info]: SVT [config]: preset / tune / pred struct : 11 / PSNR / random access"
	logPattern := regexp.MustCompile(`\.reencode-runtime-[0-9]+\.json`)
	for _, mode := range []string{"interactive", "json", "no-progress"} {
		t.Run(mode, func(t *testing.T) {
			encoderTuneCache.versions = map[string]string{}
			scoringRuntimeCache.versions = map[string]RuntimeVersions{}
			var buf bytes.Buffer
			p := &ProgressDisplay{out: &buf, live: true}
			opts := defaultProbeOptions()
			opts.TempDir = t.TempDir()
			opts.Progress, opts.JSON, opts.NoProgress = p, mode == "json", mode == "no-progress"
			check := func(run FFMpegRun) {
				t.Helper()
				want := p
				if mode != "interactive" {
					want = nil
				}
				if run.Progress != want || run.ExpectedFrames != 12 || run.ExpectedDuration != 400*time.Millisecond {
					t.Fatalf("incorrect runtime progress: %+v", run)
				}
			}
			tuneCalls, scoringCalls := 0, 0
			executeTuneCheck = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				tuneCalls++
				check(run)
				return FFMpegResult{LastFrame: 12, Stderr: banner}, nil
			}
			executeVMAF = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				scoringCalls++
				check(run)
				name := logPattern.FindString(strings.Join(run.Args, " "))
				if name == "" {
					t.Fatal("missing report path")
				}
				if err := os.WriteFile(filepath.Join(opts.TempDir, name), []byte(reportJSON(100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100)), 0o600); err != nil {
					t.Fatal(err)
				}
				return FFMpegResult{LastFrame: 12, Stderr: banner}, nil
			}
			model := resolveVMAFModel(opts, MediaInfo{Width: 1920, Height: 1080, FPS: 24})
			if _, err := detectScoringRuntime(context.Background(), opts, model); err != nil {
				t.Fatal(err)
			}
			if tuneCalls != 1 || scoringCalls != 1 {
				t.Fatal("runtime progress checks were not exercised")
			}
		})
	}
}
