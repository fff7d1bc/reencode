package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt in so the normal test suite does not require external media tools.
func requireMediaIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("REENCODE_INTEGRATION") != "1" {
		t.Skip("set REENCODE_INTEGRATION=1 to exercise ffmpeg and ffprobe")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
}

func mediaFixture(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create media fixture: %v\n%s", err, out)
	}
}

func TestMediaIntegrationRejectsAlpha(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc=size=160x96:rate=24,format=argb,colorchannelmixer=aa=0.5", "-t", "3", "-c:v", "qtrle", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, mode := range []eligibilityMode{eligibilityProbe, eligibilityEncode} {
		for _, opts := range []EncodeOptions{
			{ProbeOptions: defaultProbeOptions()},
			{CRFSet: true, CRF: 32},
			{FallbackCRFSet: true, FallbackCRF: 32},
			{ProbeOptions: ProbeOptions{ForceVideo: true}},
		} {
			result := collectEligibleInputs(ctx, mode, []string{path}, opts)
			if result.Fatal == nil || !strings.Contains(result.Fatal.Error(), "alpha") || len(result.Actionable) != 0 {
				t.Fatalf("alpha input was not rejected: %+v", result)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("source changed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected outputs: %v, %v", entries, err)
	}
}

func TestMediaIntegrationRejectsHDR(t *testing.T) {
	requireMediaIntegration(t)
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		t.Run(transfer, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "hdr.mkv")
			mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24", "-t", "1",
				"-vf", "setparams=color_trc="+transfer+":color_primaries=bt2020:colorspace=bt2020nc",
				"-c:v", "libsvtav1", "-preset", "11", "-pix_fmt", "yuv420p10le", "-color_trc", transfer, "-color_primaries", "bt2020", path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for _, mode := range []eligibilityMode{eligibilityProbe, eligibilityEncode} {
				for _, opts := range []EncodeOptions{
					{ProbeOptions: defaultProbeOptions()}, {CRFSet: true, CRF: 32},
					{FallbackCRFSet: true, FallbackCRF: 32}, {GroupCRF: true},
					{ProbeOptions: ProbeOptions{ForceVideo: true}},
				} {
					result := collectEligibleInputs(ctx, mode, []string{path}, opts)
					if result.Fatal == nil || !strings.Contains(result.Fatal.Error(), "HDR") || len(result.Actionable) != 0 {
						t.Fatalf("HDR accepted: %+v", result)
					}
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("source changed: %v", err)
			}
		})
	}
}

func TestMediaIntegrationVariableFrameRate(t *testing.T) {
	requireMediaIntegration(t)
	path := filepath.Join(t.TempDir(), "vfr.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=160x96:rate=24", "-t", "12",
		"-vf", `select=if(lt(t\,6)\,not(mod(n\,3))\,1)`, "-fps_mode", "vfr",
		"-c:v", "mpeg4", "-q:v", "2", path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts := defaultProbeOptions()
	opts.Preset, opts.NoCache, opts.NoProgress = "12", true, true
	opts.VMAFProfile = "legacy"
	source, err := probeInputMediaContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := decodeVideoFrameCount(ctx, path, source, opts); err != nil || count != 192 {
		t.Fatalf("fixture frame count = %d, %v", count, err)
	}
	search := crfSearch{info: source, options: opts}
	defer search.cleanupEncodedSamples()
	for _, window := range []struct {
		start  time.Duration
		frames int64
	}{{2100 * time.Millisecond, 16}, {7100 * time.Millisecond, 48}} {
		sample, err := search.prepareSample(ctx, SampleFile{SourcePath: path, Start: window.start, Duration: 2 * time.Second})
		if err != nil || sample.Frames != window.frames {
			t.Fatalf("VFR sample=%+v, %v", sample, err)
		}
		encoded, _, err := search.encodeSample(ctx, buildVideoArgs(source, opts.Preset, 32), sample, 1, probeProgress{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := search.scoreSample(ctx, sample, encoded, probeProgress{}); err != nil {
			t.Fatal(err)
		}
	}
	if code := runEncodeCommand(ctx, EncodeOptions{ProbeOptions: opts, CRFSet: true, CRF: 32}, []string{path}); code != 0 {
		t.Fatalf("VFR encode failed: exit %d", code)
	}
	outputPath := outputPathFor(path)
	output, err := probeMediaContext(ctx, outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := decodeVideoFrameCount(ctx, outputPath, output, opts); err != nil || count != 192 {
		t.Fatalf("output frame count = %d, %v", count, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("validated source not removed: %v", err)
	}
}

func TestMediaIntegrationRotation(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	base := filepath.Join(dir, "base.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc=size=160x96:rate=24,format=rgb24", "-t", "3", "-c:v", "qtrle", base)
	for _, rotation := range []int{90, 180, 270} {
		t.Run(fmt.Sprint(rotation), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("rotated-%d.mov", rotation))
			mediaFixture(t, "-display_rotation", fmt.Sprint(rotation), "-i", base, "-c", "copy", path)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			source, err := probeInputMediaContext(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			wantWidth, wantHeight := 96, 160
			if rotation == 180 {
				wantWidth, wantHeight = 160, 96
			}
			if source.Width != wantWidth || source.Height != wantHeight {
				t.Fatalf("source dimensions = %dx%d", source.Width, source.Height)
			}
			opts := defaultProbeOptions()
			opts.Preset, opts.NoCache, opts.NoProgress = "12", true, true
			opts.VMAFProfile = "legacy"
			// Exercise automatic VMAF probing as well as explicit-CRF encoding.
			encodeOpts := EncodeOptions{ProbeOptions: opts, CRFSet: rotation != 90, CRF: 32}
			if code := runEncodeCommand(ctx, encodeOpts, []string{path}); code != 0 {
				t.Fatalf("rotated encode failed: exit %d", code)
			}
			output, err := probeMediaContext(ctx, outputPathFor(path))
			if err != nil || output.Width != wantWidth || output.Height != wantHeight {
				t.Fatalf("output dimensions = %dx%d, err = %v", output.Width, output.Height, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("validated source not removed: %v", err)
			}
		})
	}
}

func TestMediaIntegrationFractionalRateAndStartTime(t *testing.T) {
	requireMediaIntegration(t)
	path := filepath.Join(t.TempDir(), "shifted.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=24000/1001", "-t", "6",
		"-c:v", "mpeg4", "-q:v", "2", "-output_ts_offset", "5", path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := probeInputMediaContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if info.StartTime != 5*time.Second {
		t.Fatalf("fixture start=%s", info.StartTime)
	}
	opts := defaultProbeOptions()
	opts.Preset, opts.NoCache, opts.NoProgress = "11", true, true
	opts.TempDir = filepath.Join(t.TempDir(), "VMAF's [reports], with spaces")
	search := crfSearch{info: info, options: opts}
	defer search.cleanupEncodedSamples()
	sample, err := search.prepareSample(ctx, SampleFile{SourcePath: path, Start: 2123 * time.Millisecond, Duration: 1700 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if sample.Frames != 41 {
		t.Fatalf("fractional sample has %d frames", sample.Frames)
	}
	encoded, _, err := search.encodeSample(ctx, buildVideoArgs(info, opts.Preset, 32), sample, 1, probeProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := search.scoreSample(ctx, sample, encoded, probeProgress{}); err != nil {
		t.Fatal(err)
	}
}
