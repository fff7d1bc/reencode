package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Tests of downstream behavior explicitly stub capabilities, rather than
// depending on whichever ffmpeg happens to be installed on the test host.
func stubCaptureSupport(t *testing.T) {
	t.Helper()
	old := checkCaptureSupport
	t.Cleanup(func() { checkCaptureSupport = old })
	checkCaptureSupport = func(context.Context, ProbeOptions) (string, error) { return capturePolicy, nil }
}

func TestCaptureUnsupportedClassification(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCaptureExitHelper$")
	cmd.Env = append(os.Environ(), "REENCODE_CAPTURE_EXIT_HELPER=1")
	err := cmd.Run()
	if !ordinaryExitOnly(err) {
		t.Fatalf("helper did not exit normally: %v", err)
	}
	for _, diagnostic := range []string{
		"Unknown encoder 'wrapped_avframe'",
		"Encoder (codec wrapped_avframe) not found for output stream #1:0",
		"[AVFormatContext] Requested output format 'framecrc' is not known.",
		"Unknown format 'framecrc'",
		"Invalid time base: filter",
	} {
		for _, failure := range []error{err, fmt.Errorf("wrapped: %w", errors.Join(err))} {
			if unsupportedCaptureFeature(failure, diagnostic) == nil {
				t.Fatalf("missing explicit unsupported classification: %v / %s", failure, diagnostic)
			}
		}
		for _, failure := range []error{nil, os.ErrNotExist, context.Canceled, context.DeadlineExceeded, errors.New("exit status 1"), errors.Join(err, io.ErrUnexpectedEOF)} {
			if unsupportedCaptureFeature(failure, diagnostic) != nil {
				t.Fatalf("non-exit failure masked: %v / %s", failure, diagnostic)
			}
		}
	}
	for _, diagnostic := range []string{"Invalid argument", "Unknown encoder 'libsvtav1'", "No such filter: 'libvmaf'", "framecrc: Permission denied", "unsupported runtime"} {
		if unsupportedCaptureFeature(err, diagnostic) != nil {
			t.Fatal("unrelated failure masked", diagnostic)
		}
	}
	// A killed process must not select compatibility mode, even if its stderr
	// happens to contain an unsupported-feature diagnostic.
	if runtime.GOOS == "windows" {
		return // Windows process termination is an exit code, not a Unix signal.
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestCaptureExitHelper$")
	cmd.Env = append(os.Environ(), "REENCODE_CAPTURE_EXIT_HELPER=wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); ordinaryExitOnly(err) {
		t.Fatal("signal classified as an ordinary exit", err)
	}
}

func TestCaptureExitHelper(t *testing.T) {
	if os.Getenv("REENCODE_CAPTURE_EXIT_HELPER") == "wait" {
		time.Sleep(time.Minute)
	}
	if os.Getenv("REENCODE_CAPTURE_EXIT_HELPER") == "1" {
		os.Exit(1)
	}
}

func TestCaptureSelectionAndRetry(t *testing.T) {
	old, checked, backend := checkFrameCapture, captureSupport.checked, captureSupport.backend
	t.Cleanup(func() { checkFrameCapture, captureSupport.checked, captureSupport.backend = old, checked, backend })
	for _, tc := range []struct {
		name   string
		err    error
		policy string
	}{
		{"supported", nil, capturePolicy},
		{"unsupported", &captureUnsupportedError{"unknown encoder 'wrapped_avframe'"}, inspectionPolicy},
		{"filesystem", &os.PathError{Op: "mkdir", Path: "scratch", Err: os.ErrPermission}, ""},
		{"malformed", errors.New("invalid capture timestamp"), ""},
		{"coverage", errors.New("timestamp capture failed unequal-stream coverage check"), ""},
		{"timeout", context.DeadlineExceeded, ""},
		{"unknown", errors.New("Invalid argument"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureSupport.checked, captureSupport.backend = false, ""
			calls := 0
			checkFrameCapture = func(context.Context, ProbeOptions) error { calls++; return tc.err }
			var output bytes.Buffer
			opts := ProbeOptions{Progress: &ProgressDisplay{out: &output}}
			for range 2 {
				policy, err := selectCaptureBackend(context.Background(), opts)
				if policy != tc.policy || isCaptureSetupError(err) != (tc.policy == "") {
					t.Fatal(policy, err)
				}
				if tc.policy == "" && !errors.Is(err, tc.err) {
					t.Fatal("lost setup cause", err)
				}
			}
			if (calls == 1) != (tc.policy != "") || strings.Contains(output.String(), "using bounded ffprobe") != (tc.policy == inspectionPolicy) {
				t.Fatal(calls, output.String())
			}
			if tc.policy == "" {
				checkFrameCapture = func(context.Context, ProbeOptions) error { return nil }
				if policy, err := selectCaptureBackend(context.Background(), opts); err != nil || policy != capturePolicy {
					t.Fatal("failed check poisoned retry", policy, err)
				}
			}
		})
	}
	for _, duringCheck := range []bool{false, true} {
		captureSupport.checked = false
		ctx, cancel := context.WithCancel(context.Background())
		if !duringCheck {
			cancel()
		}
		checkFrameCapture = func(context.Context, ProbeOptions) error { cancel(); return &captureUnsupportedError{"unsupported"} }
		if _, err := selectCaptureBackend(ctx, ProbeOptions{}); !errors.Is(err, context.Canceled) || isCaptureSetupError(err) || captureSupport.checked {
			t.Fatal("cancellation was cached or reclassified", err)
		}
		cancel()
	}
}

func TestCaptureSetupStopsQualityWork(t *testing.T) {
	stubCaptureSupport(t)
	fault := errors.New("broken capture scratch")
	checkCaptureSupport = func(context.Context, ProbeOptions) (string, error) { return "", &captureSetupError{fault} }
	oldRuntime, oldRun := checkScoringRuntime, executeFinalFFmpeg
	t.Cleanup(func() { checkScoringRuntime, executeFinalFFmpeg = oldRuntime, oldRun })
	checkScoringRuntime = func(context.Context, ProbeOptions, VMAFModelInfo) (RuntimeVersions, error) {
		t.Fatal("model runtime check ran before capture setup")
		return RuntimeVersions{}, nil
	}
	executeFinalFFmpeg = func(context.Context, FFMpegRun) (FFMpegResult, error) {
		t.Fatal("final encoding started after capture setup failure")
		return FFMpegResult{}, nil
	}
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), FallbackCRFSet: true, FallbackCRF: 32}
	info := validationSourceInfo()
	crf, probe, err := chooseCRF(context.Background(), opts, info)
	if crf != 0 || probe != nil || !errors.Is(err, fault) || !isCaptureSetupError(err) {
		t.Fatal(crf, probe, err)
	}
	if err := groupFallbackOrError(context.Background(), opts, nil, fmt.Errorf("group probe: %w", err)); !errors.Is(err, fault) || !isCaptureSetupError(err) {
		t.Fatal(err)
	}
	comparison, err := compareInput(context.Background(), CompareOptions{ProbeOptions: opts.ProbeOptions, Profiles: []string{"legacy", "v1"}, Tunes: []string{"psnr", "vq"}}, info)
	if !errors.Is(err, fault) || len(comparison.Results) != 1 || comparison.Results[0].Error == "" {
		t.Fatal(comparison, err)
	}
	result, _, _, err := probeMediaFile(context.Background(), opts.ProbeOptions, info, false)
	if !errors.Is(err, fault) || result.TimingPolicy != "" {
		t.Fatal(result, err)
	}
	data, _ := json.Marshal(result)
	if bytes.Contains(data, []byte(`"timing_policy"`)) {
		t.Fatal("unselected policy present", string(data))
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "source.mkv")
	output := outputPathFor(path)
	for _, file := range []string{path, output} {
		if err := os.WriteFile(file, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	info.Path = path
	src, _ := os.Stat(path)
	dst, _ := os.Stat(output)
	input := eligibleInput{File: path, Info: info, Source: snapshotSource(src), Output: output, Destination: snapshotDestination(dst)}
	opts.CRFSet, opts.CRF, opts.VerifyQuality, opts.Overwrite = true, 32, true, true
	if err := encodeOneWithCRF(context.Background(), opts, input, 32, nil); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	for _, file := range []string{path, output} {
		if data, err := os.ReadFile(file); err != nil || string(data) != "unchanged" {
			t.Fatal(file, string(data), err)
		}
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatal("created temporary final output before precheck", files)
	}
}

func TestCachedTimingPolicyCompatibility(t *testing.T) {
	for _, policy := range []string{capturePolicy, inspectionPolicy} {
		for _, stored := range []string{"", "obsolete-display-value"} {
			opts := normalizedProbeCacheOptions(defaultProbeOptions())
			opts.TimingPolicy = policy
			key, err := probeCacheOptionsKey(opts)
			if err != nil {
				t.Fatal(err)
			}
			handle := newProbeCacheHandle(t.TempDir(), "binary", "tools", inputFingerprint{}, "input", opts, key, "input.mkv")
			if err := storeProbeCache(handle, ProbeResult{Success: true, TimingPolicy: stored}); err != nil {
				t.Fatal(err)
			}
			result, ok, err := loadProbeCache(handle, "input.mkv")
			if err != nil || !ok || !result.CacheHit || result.TimingPolicy != policy {
				t.Fatal(result, ok, err)
			}
		}
	}
}
