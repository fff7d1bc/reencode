package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInspectionBudget(t *testing.T) {
	for _, cpus := range []int{1, 2, 3, 4, 7, 8, 16, 31, 32, 64} {
		for _, workers := range []int{1, 4, 16, 100} {
			b := newInspectionBudget(cpus, workers)
			if b.threads < 1 || cap(b.slots) < 1 || cap(b.slots) > workers || b.threads*cap(b.slots) > max(1, cpus/4) {
				t.Fatalf("cpus=%d workers=%d: threads=%d slots=%d", cpus, workers, b.threads, cap(b.slots))
			}
		}
	}
	for _, tt := range []struct{ workers, slots, threads int }{{1, 1, 8}, {4, 4, 2}, {16, 8, 1}} {
		b := newInspectionBudget(32, tt.workers)
		if cap(b.slots) != tt.slots || b.threads != tt.threads {
			t.Fatal(b)
		}
	}
}

func inspectionHelperCommand(ctx context.Context, data string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestInspectionHelperProcess")
	cmd.Env = append(os.Environ(), "REENCODE_INSPECTION_HELPER=1", "REENCODE_INSPECTION_DATA="+data)
	return cmd
}

func TestInspectionHelperProcess(t *testing.T) {
	if os.Getenv("REENCODE_INSPECTION_HELPER") != "1" {
		return
	}
	fmt.Print(os.Getenv("REENCODE_INSPECTION_DATA"))
	os.Exit(0)
}

func TestInspectionBudgetBoundsConcurrencyAndCancelsWaiters(t *testing.T) {
	old := inspectionCommandContext
	t.Cleanup(func() { inspectionCommandContext = old })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b := newInspectionBudget(8, 8)
	ctx = context.WithValue(ctx, inspectionBudgetKey{}, b)
	entered := make(chan struct{}, 8)
	inspectionCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		entered <- struct{}{}
		<-ctx.Done()
		return inspectionHelperCommand(ctx, "unused")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := frameProbeOutput(ctx, 0, "-show_frames")
			errs <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Error("inspection slots were not used")
		}
	}
	select {
	case <-entered:
		t.Error("too many decoders admitted")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("waiter did not cancel: %v", err)
		}
	}
	if len(b.slots) != 0 {
		t.Fatal("inspection slot leaked")
	}
}

func TestFrameInspectionArgs(t *testing.T) {
	old := inspectionCommandContext
	t.Cleanup(func() { inspectionCommandContext = old })
	ctx := context.WithValue(context.Background(), inspectionBudgetKey{}, newInspectionBudget(32, 4))
	var calls [][]string
	inspectionCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "ffprobe" || len(args) < 2 || args[0] != "-threads" || args[1] != "2" {
			t.Fatalf("unbounded frame probe: %s %v", name, args)
		}
		calls = append(calls, args)
		return inspectionHelperCommand(ctx, string(timelineJSON("228", "228.04", "228.08")))
	}
	if _, err := inspectTimeline(ctx, "source.mkv", 5*time.Second, 223*time.Second, 20*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls[0], " "); !strings.Contains(got, "-read_intervals 228%250") || !strings.Contains(got, ":frame_side_data=") {
		t.Fatal(got)
	}
	if _, err := inspectInitialHDR(ctx, "source.mkv"); err != nil {
		t.Fatal(err)
	}
	if _, err := firstVideoTime(ctx, MediaInfo{Path: "source.mkv"}); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls[1:] {
		if !strings.Contains(strings.Join(call, " "), "-read_intervals %+#64") {
			t.Fatal("prefix decode is not bounded", call)
		}
	}
}

func TestEligibilitySkipsBeforeFrameDecoding(t *testing.T) {
	oldMeta, oldHDR := inspectEligibilityMetadata, inspectEligibilityHDR
	t.Cleanup(func() { inspectEligibilityMetadata, inspectEligibilityHDR = oldMeta, oldHDR })
	var metaCalls, frameCalls atomic.Int64
	inspectEligibilityMetadata = func(_ context.Context, path string, _ bool) (MediaInfo, error) {
		metaCalls.Add(1)
		return MediaInfo{Path: path, VideoCodec: "av1", VideoIndex: 0}, nil
	}
	inspectEligibilityHDR = func(context.Context, string) (string, error) {
		frameCalls.Add(1)
		return "", nil
	}
	dir := t.TempDir()
	files := make([]string, 3000)
	for i := range files {
		files[i] = filepath.Join(dir, strconv.Itoa(i)+".mkv")
		if err := os.WriteFile(files[i], []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	result := collectEligibleInputs(context.Background(), eligibilityEncode, files, opts)
	if result.Fatal != nil || len(result.Skipped) != len(files) || len(result.Actionable) != 0 || frameCalls.Load() != 0 || metaCalls.Load() != int64(len(files)) {
		t.Fatalf("batch: skipped=%d metadata=%d decoded=%d err=%v", len(result.Skipped), metaCalls.Load(), frameCalls.Load(), result.Fatal)
	}
	opts.ForceReencode = true
	result = collectEligibleInputs(context.Background(), eligibilityEncode, files[:1], opts)
	if result.Fatal != nil || len(result.Actionable) != 1 || frameCalls.Load() != 1 {
		t.Fatal("forced reencode bypassed frame validation", result.Fatal)
	}
	// Explicit probing of an already-AV1 file still needs quality safeguards.
	result = collectEligibleInputs(context.Background(), eligibilityProbe, files[:1], opts)
	if result.Fatal != nil || len(result.Actionable) != 1 || frameCalls.Load() != 2 {
		t.Fatal("probe bypassed frame validation", result.Fatal)
	}
}

func TestInspectedMetadataReuseRejectsChangedSources(t *testing.T) {
	stubCaptureSupport(t)
	oldMeta, oldHDR, oldRuntime := inspectEligibilityMetadata, inspectEligibilityHDR, checkScoringRuntime
	t.Cleanup(func() {
		inspectEligibilityMetadata, inspectEligibilityHDR, checkScoringRuntime = oldMeta, oldHDR, oldRuntime
	})
	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata, frames := 0, 0
	inspectEligibilityMetadata = func(context.Context, string, bool) (MediaInfo, error) {
		metadata++
		return MediaInfo{Path: path, VideoCodec: "hevc", VideoIndex: 0}, nil
	}
	inspectEligibilityHDR = func(context.Context, string) (string, error) { frames++; return "", nil }
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	result := collectEligibleInputs(context.Background(), eligibilityEncode, []string{path}, opts)
	if result.Fatal != nil || len(result.Actionable) != 1 {
		t.Fatal(result.Fatal)
	}
	input := result.Actionable[0]
	stop := errors.New("reached scoring with retained metadata")
	checkScoringRuntime = func(context.Context, ProbeOptions, VMAFModelInfo) (RuntimeVersions, error) {
		return RuntimeVersions{}, stop
	}
	if _, err := probeInspectedFile(context.Background(), opts.ProbeOptions, input.Info, input.Source); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if _, _, err := chooseCRF(context.Background(), opts, input.Info); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if metadata != 1 || frames != 1 {
		t.Fatalf("repeated inspection: metadata=%d frames=%d", metadata, frames)
	}
	if err := os.WriteFile(path, []byte("changed source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateEncodePaths(opts, []string{path}, result.Actionable); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatal("planning accepted stale inspection", err)
	}
	if _, err := probeInspectedFile(context.Background(), opts.ProbeOptions, input.Info, input.Source); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatal("probing accepted stale inspection", err)
	}
}

func TestMediaIntegrationThreadedTimeline(t *testing.T) {
	requireMediaIntegration(t)
	path := filepath.Join(t.TempDir(), "fractional.mkv")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24000/1001", "-t", "2", "-c:v", "libsvtav1", "-preset", "11", path)
	for _, full := range []bool{false, true} {
		var baseline frameTimeline
		for _, cpus := range []int{4, 16, 32} {
			ctx := context.WithValue(context.Background(), inspectionBudgetKey{}, newInspectionBudget(cpus, 1))
			got, err := inspectTimeline(ctx, path, 0, 357*time.Millisecond, 800*time.Millisecond, full)
			if err != nil {
				t.Fatal(err)
			}
			if cpus == 4 {
				baseline = got
			} else if !reflect.DeepEqual(got, baseline) {
				t.Fatalf("threads changed frame coverage/timing: %+v vs %+v", got, baseline)
			}
		}
	}
}
