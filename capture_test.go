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

func captureFixture() string {
	return "#tb 0: 1001/24000\n#media_type 0: video\n#codec_id 0: wrapped_avframe\n" +
		"#tb 1: 1/1000\n#media_type 1: video\n#codec_id 1: wrapped_avframe\n" +
		"0, 0, 0, 1, 424, ignored\n1, 0, 0, 42, 424, ignored\n" +
		"0, 3, 3, 1, 424, ignored, S=1, 10\n1, 125, 125, 42, 424, ignored\n"
}

func TestFrameCaptureRationalVFRAndValidation(t *testing.T) {
	frames, err := readFrameCapture(strings.NewReader(captureFixture()))
	if err != nil || len(frames[0]) != 2 || frames[0][1].PTS != 125125*time.Microsecond || frames[0][0].Duration != 41708333*time.Nanosecond {
		t.Fatal(frames, err)
	}
	for _, bad := range []string{
		strings.Replace(captureFixture(), "1001/24000", "0/0", 1),
		strings.Replace(captureFixture(), "wrapped_avframe", "rawvideo", 1),
		strings.Replace(captureFixture(), "0, 3, 3", "0, 3, 0", 1),
		strings.Replace(captureFixture(), "0, 3, 3", "2, 3, 3", 1),
		strings.Replace(captureFixture(), "0, 3, 3", "0, 3, N/A", 1),
		strings.Replace(captureFixture(), "0, 3, 3", "0, 3, 9223372036854775807", 1),
		captureFixture() + "0, 5",
		"#tb 0: 1/24\n",
	} {
		if _, err := readFrameCapture(strings.NewReader(bad)); err == nil {
			t.Fatal("invalid capture accepted", bad)
		}
	}
}

func TestCapturedTimelinesRejectCoverageAndDuplicates(t *testing.T) {
	ref, err := timelineFromFrames([]decodedFrame{{0, time.Second}, {time.Second, time.Second}, {2 * time.Second, time.Second}}, 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, frames := range [][]decodedFrame{
		{{0, time.Second}, {time.Second, time.Second}},
		{{0, time.Second}, {time.Second, time.Second}, {2 * time.Second, time.Second}, {3 * time.Second, time.Second}},
		{{0, time.Second}, {time.Second, time.Second}, {time.Second, time.Second}},
	} {
		dis, err := timelineFromFrames(frames, 0, 0, true)
		if err == nil {
			err = validateTimelines(ref, dis)
		}
		if err == nil {
			t.Fatal("invalid coverage accepted")
		}
	}
}

func TestCaptureCapabilityFallbackAndCancellation(t *testing.T) {
	old, checked, backend := checkFrameCapture, captureSupport.checked, captureSupport.backend
	defer func() { checkFrameCapture, captureSupport.checked, captureSupport.backend = old, checked, backend }()
	captureSupport.checked = false
	calls := 0
	checkFrameCapture = func(context.Context, ProbeOptions) error { calls++; return fmt.Errorf("unsupported encoder") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := selectCaptureBackend(ctx, ProbeOptions{}); err == nil || calls != 0 {
		t.Fatal("cancellation ignored")
	}
	for range 2 {
		backend, err := selectCaptureBackend(context.Background(), ProbeOptions{})
		if err != nil || backend != inspectionPolicy {
			t.Fatal(backend, err)
		}
	}
	if calls != 1 {
		t.Fatal("capability check repeated", calls)
	}
}

func TestMediaIntegrationFrameCapture(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	if err := smokeFrameCapture(context.Background(), ProbeOptions{TempDir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, rate := range []string{"24", "24000/1001"} {
		path := filepath.Join(dir, "capture.crc")
		// Preserve sparse PTS and non-zero initial timestamps on both branches.
		filter := "[0:v]select='not(mod(n,3))',setpts=PTS+2/TB,split[capture_dis][a];[1:v]select='not(mod(n,3))',setpts=PTS+2/TB,split[capture_ref][b];[a][b]psnr=shortest=true[quality]"
		args := []string{"-y", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=" + rate + ":duration=0.5", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=" + rate + ":duration=0.5", "-filter_complex", filter, "-map", "[quality]", "-fps_mode", "passthrough", "-f", "null", nullOutputPath()}
		args = append(args, captureOutputArgs(path)...)
		if _, err := runFFmpeg(context.Background(), FFMpegRun{Args: args}); err != nil {
			t.Fatal(err)
		}
		frames, err := loadFrameCapture(path)
		if err != nil || len(frames[0]) != 4 || frames[0][0].PTS < 1900*time.Millisecond || frames[0][1].PTS-frames[0][0].PTS < 125*time.Millisecond {
			t.Fatal(fmt.Sprint(frames), err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("capture scratch leak", entries, err)
	}
}
