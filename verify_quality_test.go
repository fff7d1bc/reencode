package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyQualityCLIAndTarget(t *testing.T) {
	for _, args := range [][]string{{"--verify-quality", "movie"}, {"--verify-quality", "--crf", "32", "movie"}, {"--verify-quality", "--fallback-crf", "32", "movie"}, {"--verify-quality", "--group-crf", "movie"}} {
		opts, _, err := parseEncodeArgs(args)
		if err != nil || !opts.VerifyQuality || !encodeFFmpegRequirements(opts).VMAF {
			t.Fatal(opts, err)
		}
	}
	opts, _, err := parseEncodeArgs([]string{"--crf", "32", "movie"})
	if err != nil || opts.VerifyQuality || encodeFFmpegRequirements(opts).VMAF {
		t.Fatal(opts, err)
	}
	if verificationTarget(opts.ProbeOptions, nil) != 95 || verificationTarget(opts.ProbeOptions, &ProbeResult{Success: true, EffectiveTarget: 96}) != 96 {
		t.Fatal("incorrect target")
	}
}

func TestVerifyQualityRules(t *testing.T) {
	for _, tt := range []struct {
		name            string
		values          []float64
		noOutlier, pass bool
	}{
		{"pass", []float64{96, 95}, false, true},
		{"mean", []float64{94, 94}, false, false},
		{"floor", []float64{99, 90}, false, false},
		{"confirmed", []float64{97, 93.5, 95, 95}, false, true},
		{"unconfirmed", []float64{97, 93.5, 93, 95}, false, false},
		{"disabled", []float64{97, 93.5}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := defaultProbeOptions()
			opts.NoOutlierCheck = tt.noOutlier
			search := crfSearch{info: MediaInfo{Duration: time.Minute}, options: opts, samples: []SampleFile{{Start: 10 * time.Second, Duration: 2 * time.Second}, {Start: 30 * time.Second, Duration: 2 * time.Second}}}
			i := 0
			attempt, err := verifyQualitySamples(context.Background(), &search, 95, func(_ context.Context, sample SampleFile) (SampleDiagnostics, error) {
				if i >= len(tt.values) {
					t.Fatal("unexpected neighboring window")
				}
				v := tt.values[i]
				i++
				return SampleDiagnostics{Score: v, Frames: 48, DurationSeconds: 2}, nil
			})
			if (err == nil) != tt.pass || i != len(tt.values) {
				t.Fatal(attempt, err, i)
			}
		})
	}
}

func TestVerificationWindowUsesMeasuredEndpoints(t *testing.T) {
	sample := SampleFile{Start: time.Second, Duration: time.Second, Timeline: &frameTimeline{Start: 1040 * time.Millisecond, Times: []time.Duration{0, 40 * time.Millisecond, 80 * time.Millisecond}}}
	window := verificationWindow(sample, MediaInfo{StartTime: -7 * time.Millisecond}, 7*time.Millisecond)
	if window.Start != 1045*time.Millisecond || window.Duration != 84*time.Millisecond || window.Origin != -7*time.Millisecond {
		t.Fatal(window)
	}
	sample.FullPass = true
	if got := verificationWindow(sample, MediaInfo{}, 0); got.Start != 0 || got.Duration != 0 {
		t.Fatal("full pass was cut", got)
	}
}

func TestVerifyQualityRejectsIncompleteAndCanceledSamples(t *testing.T) {
	search := crfSearch{options: defaultProbeOptions(), samples: []SampleFile{{Duration: time.Second}}}
	if _, err := verifyQualitySamples(context.Background(), &search, 95, func(context.Context, SampleFile) (SampleDiagnostics, error) { return SampleDiagnostics{}, nil }); err == nil {
		t.Fatal("missing diagnostic accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := verifyQualitySamples(ctx, &search, 95, func(context.Context, SampleFile) (SampleDiagnostics, error) {
		cancel()
		return SampleDiagnostics{Frames: 24, DurationSeconds: 1, Score: 100}, nil
	}); err != context.Canceled {
		t.Fatal(err)
	}
}

func TestVerifyQualityPublicationBarrier(t *testing.T) {
	oldRun, oldInspect, oldVerify, oldRuntime := executeFinalFFmpeg, inspectFinalMedia, verifyEncodedQuality, checkScoringRuntime
	defer func() {
		executeFinalFFmpeg, inspectFinalMedia, verifyEncodedQuality, checkScoringRuntime = oldRun, oldInspect, oldVerify, oldRuntime
	}()
	checkScoringRuntime = func(context.Context, ProbeOptions, VMAFModelInfo) (RuntimeVersions, error) {
		return RuntimeVersions{LibVMAF: "3.2.1", SVTAV1: "v4.2.0"}, nil
	}
	for _, mode := range []string{"pass", "keep-source", "quality", "missing-score", "alignment", "interrupt", "structural", "group"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.mkv")
			output := outputPathFor(path)
			if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(output, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			srcStat, _ := os.Stat(path)
			destStat, _ := os.Stat(output)
			info := validationSourceInfo()
			info.Path = path
			input := eligibleInput{File: path, Info: info, Source: snapshotSource(srcStat), Output: output, Destination: snapshotDestination(destStat)}
			opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), VerifyQuality: true, Overwrite: true, CRFSet: true, CRF: 32, KeepSource: mode == "keep-source"}
			opts.ProbeOptions.KeepTemp = true
			validated, verified := false, false
			executeFinalFFmpeg = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				return FFMpegResult{}, os.WriteFile(run.Args[len(run.Args)-1], []byte("new"), 0o600)
			}
			inspectFinalMedia = func(context.Context, string) (MediaInfo, error) {
				validated = true
				if mode == "structural" {
					return MediaInfo{}, fmt.Errorf("bad structure")
				}
				return validationOutputInfo(), nil
			}
			verifyEncodedQuality = func(ctx context.Context, _ ProbeOptions, _ MediaInfo, tmp string, _ *ProbeResult) error {
				verified = true
				if !validated || !strings.HasPrefix(filepath.Base(tmp), ".") {
					t.Fatal("verification order/path")
				}
				if data, _ := os.ReadFile(output); string(data) != "old" {
					t.Fatal("published before verification")
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("source removed before verification")
				}
				switch mode {
				case "quality", "missing-score", "alignment", "group":
					return fmt.Errorf("verification failure")
				case "interrupt":
					return context.Canceled
				}
				return nil
			}
			var err error
			if mode == "group" {
				err = encodeGroup(context.Background(), opts, []eligibleInput{input})
			} else {
				err = encodeOneWithCRF(context.Background(), opts, input, 32, nil)
			}
			pass := mode == "pass" || mode == "keep-source"
			if (err == nil) != pass || verified == (mode == "structural") {
				t.Fatal(err, verified)
			}
			want := "old"
			if pass {
				want = "new"
			}
			if data, _ := os.ReadFile(output); string(data) != want {
				t.Fatal(string(data))
			}
			_, err = os.Stat(path)
			if (err == nil) != (!pass || opts.KeepSource) {
				t.Fatal("source retention", err)
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					t.Fatal("temp leak", entry.Name())
				}
			}
		})
	}
}

func TestMediaIntegrationFinalQuality(t *testing.T) {
	requireMediaIntegration(t)
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprint(full), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "final.mp4")
			mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24000/1001", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000", "-t", "4", "-vf", `select=if(lt(t\,2)\,not(mod(n\,2))\,1)`, "-fps_mode", "vfr", "-c:v", "mpeg4", "-q:v", "2", "-c:a", "flac", "-strict", "-2", "-output_ts_offset", "5", path)
			opts := defaultProbeOptions()
			opts.Preset = "11"
			opts.NoCache = true
			opts.NoProgress = true
			opts.TargetVMAF = 80
			opts.FloorVMAF = 75
			if !full {
				opts.Samples = 2
				opts.SampleDuration = 700 * time.Millisecond
			}
			if code := runEncodeCommand(context.Background(), EncodeOptions{ProbeOptions: opts, CRFSet: true, CRF: 20, KeepSource: true, VerifyQuality: true}, []string{path}); code != 0 {
				t.Fatalf("verify failed: %d", code)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("source changed", err)
			}
			if _, err := os.Stat(outputPathFor(path)); err != nil {
				t.Fatal("no output", err)
			}
			if !full {
				previous, err := os.ReadFile(outputPathFor(path))
				if err != nil {
					t.Fatal(err)
				}
				opts.TargetVMAF, opts.FloorVMAF, opts.KeepTemp = 100, 100, true
				if code := runEncodeCommand(context.Background(), EncodeOptions{ProbeOptions: opts, CRFSet: true, CRF: 70, Overwrite: true, VerifyQuality: true}, []string{path}); code != 1 {
					t.Fatalf("damaged encode accepted: %d", code)
				}
				after, err := os.ReadFile(outputPathFor(path))
				if err != nil || string(previous) != string(after) {
					t.Fatal("failed verification replaced output", err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Fatal("failed verification removed source", err)
				}
				entries, _ := os.ReadDir(filepath.Dir(path))
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".") {
						t.Fatal("failed final temp retained", entry.Name())
					}
				}
			}
		})
	}
}
