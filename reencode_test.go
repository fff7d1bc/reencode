package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseFrameRate(t *testing.T) {
	tests := map[string]float64{
		"30000/1001": 30000.0 / 1001.0,
		"24/1":       24,
		"25":         25,
		"0/0":        0,
		"":           0,
	}
	for input, want := range tests {
		got := parseFrameRate(input)
		if math.Abs(got-want) > 0.000001 {
			t.Fatalf("parseFrameRate(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestCandidateVideoByContentAcceptsMP4(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-extension")
	data := []byte{
		0x00, 0x00, 0x00, 0x18,
		'f', 't', 'y', 'p',
		'i', 's', 'o', 'm',
		0x00, 0x00, 0x00, 0x00,
		'm', 'p', '4', '1',
		'i', 's', 'o', 'm',
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := candidateVideoByContent(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("mp4 signature should be accepted")
	}
}

func TestCandidateVideoByContentAcceptsWebM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "video.bin")
	if err := os.WriteFile(path, []byte{0x1A, 0x45, 0xDF, 0xA3, 0x93, 0x42, 0x82, 0x88}, 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := candidateVideoByContent(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("webm signature should be accepted")
	}
}

func TestCandidateVideoByContentSkipsNonVideo(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"image": {0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'},
		"text":  []byte("hello\n"),
		"empty": {},
	}
	for name, data := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		ok, err := candidateVideoByContent(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ok {
			t.Fatalf("%s should not be accepted as video", name)
		}
	}
}

func TestCandidateVideoByContentSkipsDirectory(t *testing.T) {
	ok, err := candidateVideoByContent(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("directory should not be accepted as video")
	}
}

func TestProbeInputMediaNotVideoErrorUsesBasename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "image.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1A, '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := probeInputMedia(path)
	if !errors.Is(err, errNotVideoFile) {
		t.Fatalf("error = %v, want errNotVideoFile", err)
	}
	text := err.Error()
	if !contains(text, "image.png") || contains(text, dir) {
		t.Fatalf("error should use basename only: %q", text)
	}
}

func TestReserveFinalTempPathIsUniqueAndHidden(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "movie.mp4")
	first, output, err := reserveFinalTempPath(input)
	if err != nil {
		t.Fatal(err)
	}
	second, output2, err := reserveFinalTempPath(input)
	if err != nil {
		t.Fatal(err)
	}
	if output != filepath.Join(dir, "movie_[e-av1].mkv") || output2 != output {
		t.Fatalf("bad final output paths: %q %q", output, output2)
	}
	if first == second {
		t.Fatalf("final temp paths should be unique: %q", first)
	}
	for _, path := range []string{first, second} {
		base := filepath.Base(path)
		if !contains(base, ".movie_[e-av1].") || !contains(base, ".tmp.mkv") {
			t.Fatalf("bad final temp name: %q", path)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved final temp path should not exist yet: %q err=%v", path, err)
		}
	}
}

func TestReserveSampleTempPathIsUniqueAndHidden(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(t.TempDir(), "episode.mkv")
	first, err := reserveSampleTempPath(input, dir, 3, 24.25)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reserveSampleTempPath(input, dir, 3, 24.25)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("sample temp paths should be unique: %q", first)
	}
	for _, path := range []string{first, second} {
		if filepath.Dir(path) != dir {
			t.Fatalf("sample temp dir = %q, want %q", filepath.Dir(path), dir)
		}
		base := filepath.Base(path)
		for _, want := range []string{".episode.sample3.", ".crf24.25.av1.mkv"} {
			if !contains(base, want) {
				t.Fatalf("sample temp name %q missing %q", base, want)
			}
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reserved sample temp path should not exist yet: %q err=%v", path, err)
		}
	}
}

func TestRunProbeCommandStopsImmediatelyWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := runProbeCommand(ctx, defaultProbeOptions(), []string{"a.mkv", "b.mkv"}); got != 130 {
		t.Fatalf("exit code = %d, want 130", got)
	}
}

func TestRunEncodeCommandStopsImmediatelyWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), CRFSet: true, DryRun: true}
	if got := runEncodeCommand(ctx, opts, []string{"a.mkv", "b.mkv"}); got != 130 {
		t.Fatalf("exit code = %d, want 130", got)
	}
}

func TestFileSHA512MatchesIdenticalContent(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "renamed.mkv")
	if err := os.WriteFile(a, []byte("same media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("same media"), 0o644); err != nil {
		t.Fatal(err)
	}
	ha, err := fileSHA512(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := fileSHA512(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("same content hashes differ: %s != %s", ha, hb)
	}
}

func TestSampledFileSHA512SmallFileUsesFullContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.bin")
	data := []byte("small file content")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sampledFileSHA512(path, int64(len(data)), 8)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fileSHA512(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("sampled hash = %s, want full hash %s", got, want)
	}
}

func TestSampledFileSHA512LargeFileSamplesRegions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	data := bytes.Repeat([]byte{'a'}, 100)
	data[0] = 'f'
	data[50] = 'm'
	data[99] = 'l'
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := sampledFileSHA512(path, int64(len(data)), 10)
	if err != nil {
		t.Fatal(err)
	}
	data[50] = 'x'
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := sampledFileSHA512(path, int64(len(data)), 10)
	if err != nil {
		t.Fatal(err)
	}
	if base == changed {
		t.Fatalf("middle sampled-region change did not affect sampled hash")
	}
}

func TestFastInputFingerprintIncludesMtimeAndMedia(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "media.mkv")
	if err := os.WriteFile(path, []byte("media content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := MediaInfo{
		Duration:    10 * time.Second,
		FPS:         24,
		TotalFrames: 240,
		VideoBytes:  1234,
		VideoCodec:  "h264",
		PixelFormat: "yuv420p",
		Width:       1920,
		Height:      1080,
	}
	first, err := fastInputFingerprint(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if first.DurationNS != int64(info.Duration) || first.VideoCodec != "h264" || first.Width != 1920 {
		t.Fatalf("fingerprint missing media fields: %+v", first)
	}
	nextTime := time.Unix(100, 123)
	if err := os.Chtimes(path, nextTime, nextTime); err != nil {
		t.Fatal(err)
	}
	second, err := fastInputFingerprint(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("mtime change did not affect fingerprint")
	}
	info.VideoCodec = "hevc"
	third, err := fastInputFingerprint(path, info)
	if err != nil {
		t.Fatal(err)
	}
	if second == third {
		t.Fatalf("media metadata change did not affect fingerprint")
	}
}

func TestProbeCacheOptionsIgnoreNonAffectingFields(t *testing.T) {
	a := defaultProbeOptions()
	a.JSON = true
	a.TempDir = "/tmp/a"
	a.KeepTemp = true
	a.NoProgress = true
	a.Verbose = true
	a.StallTimeout = time.Second
	b := defaultProbeOptions()
	b.JSON = false
	b.TempDir = "/tmp/b"
	b.KeepTemp = false
	b.NoProgress = false
	b.Verbose = false
	b.StallTimeout = time.Hour
	if normalizedProbeCacheOptions(a) != normalizedProbeCacheOptions(b) {
		t.Fatalf("non-affecting fields changed cache options")
	}
}

func TestProbeCacheOptionsIncludeAffectingFields(t *testing.T) {
	base := defaultProbeOptions()
	changed := base
	changed.TargetVMAF = 96
	if normalizedProbeCacheOptions(base) == normalizedProbeCacheOptions(changed) {
		t.Fatalf("target VMAF should affect cache options")
	}
	changed = base
	changed.SampleDuration = 30 * time.Second
	if normalizedProbeCacheOptions(base) == normalizedProbeCacheOptions(changed) {
		t.Fatalf("sample duration should affect cache options")
	}
	changed = base
	changed.NoOutlierCheck = true
	if normalizedProbeCacheOptions(base) == normalizedProbeCacheOptions(changed) {
		t.Fatalf("outlier setting should affect cache options")
	}
}

func TestProbeCacheStoreLoadRewritesFile(t *testing.T) {
	dir := t.TempDir()
	opts := normalizedProbeCacheOptions(defaultProbeOptions())
	key, err := probeCacheOptionsKey(opts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	handle := newProbeCacheHandle(dir, "binary", fingerprint, fingerprintKey, opts, key, "/old/path.mkv")
	result := ProbeResult{File: "/old/path.mkv", Success: true, CRF: 24.25, TargetVMAF: 95, FloorVMAF: 94}
	if err := storeProbeCache(handle, result); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadProbeCache(handle, "/new/path.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if got.File != "/new/path.mkv" {
		t.Fatalf("cached file = %q, want current path", got.File)
	}
	if got.CRF != result.CRF {
		t.Fatalf("cached CRF = %v, want %v", got.CRF, result.CRF)
	}
}

func TestProbeCacheRejectsBinaryMismatch(t *testing.T) {
	dir := t.TempDir()
	opts := normalizedProbeCacheOptions(defaultProbeOptions())
	key, err := probeCacheOptionsKey(opts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	stored := newProbeCacheHandle(dir, "binary-a", fingerprint, fingerprintKey, opts, key, "a.mkv")
	if err := storeProbeCache(stored, ProbeResult{File: "a.mkv", Success: true, CRF: 24.25}); err != nil {
		t.Fatal(err)
	}
	lookup := newProbeCacheHandle(dir, "binary-b", fingerprint, fingerprintKey, opts, key, "a.mkv")
	if _, ok, err := loadProbeCache(lookup, "a.mkv"); err != nil || ok {
		t.Fatalf("binary mismatch load = ok %v err %v, want miss nil", ok, err)
	}
}

func TestProbeCacheStoreLoadPreservesAttempts(t *testing.T) {
	dir := t.TempDir()
	opts := normalizedProbeCacheOptions(defaultProbeOptions())
	key, err := probeCacheOptionsKey(opts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	handle := newProbeCacheHandle(dir, "binary", fingerprint, fingerprintKey, opts, key, "a.mkv")
	result := ProbeResult{
		File:    "a.mkv",
		Success: true,
		CRF:     24.25,
		Attempts: []ProbeAttempt{
			{CRF: 24.25, Score: 95.1, WorstSampleScore: 94.2},
			{CRF: 25.25, Score: 94.8, WorstSampleScore: 93.9},
		},
	}
	if err := storeProbeCache(handle, result); err != nil {
		t.Fatal(err)
	}
	got, ok, err := loadProbeCache(handle, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if len(got.Attempts) != 2 || got.Attempts[0].CRF != 24.25 || got.Attempts[1].CRF != 25.25 {
		t.Fatalf("attempts not preserved: %+v", got.Attempts)
	}
}

func TestProbeCacheNoCacheBypassesPrepare(t *testing.T) {
	handle, err := prepareProbeCache(ProbeOptions{NoCache: true}, "missing.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if handle != nil {
		t.Fatalf("no-cache returned handle: %+v", handle)
	}
}

func TestParseProbeCacheFlags(t *testing.T) {
	opts, files, err := parseProbeArgs([]string{"--no-cache", "--refresh-cache", "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.mkv" {
		t.Fatalf("files = %v", files)
	}
	if !opts.NoCache || !opts.RefreshCache {
		t.Fatalf("cache flags not parsed: %+v", opts)
	}
}

func TestParseEncodeCacheFlags(t *testing.T) {
	opts, files, err := parseEncodeArgs([]string{"--no-cache", "--refresh-cache", "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.mkv" {
		t.Fatalf("files = %v", files)
	}
	if !opts.ProbeOptions.NoCache || !opts.ProbeOptions.RefreshCache {
		t.Fatalf("cache flags not parsed: %+v", opts.ProbeOptions)
	}
}

func TestDefaultPresetIgnoresEnvironment(t *testing.T) {
	t.Setenv("REENCODE_PRESET", "8")
	opts := defaultProbeOptions()
	if opts.Preset != "4" {
		t.Fatalf("preset = %q, want 4", opts.Preset)
	}
}

func TestParseEncodeCRFFlags(t *testing.T) {
	opts, files, err := parseEncodeArgs([]string{"--crf", "24.25", "--fallback-crf", "32", "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.mkv" {
		t.Fatalf("files = %v", files)
	}
	if !opts.CRFSet || opts.CRF != 24.25 {
		t.Fatalf("crf flag not parsed: %+v", opts)
	}
	if !opts.FallbackCRFSet || opts.FallbackCRF != 32 {
		t.Fatalf("fallback flag not parsed: %+v", opts)
	}
}

func TestParseEncodeForceReencodeFlag(t *testing.T) {
	opts, _, err := parseEncodeArgs([]string{"--force-reencode", "a.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.ForceReencode {
		t.Fatalf("force reencode flag not parsed: %+v", opts)
	}
}

func TestMatroskaAV1SkipHonorsForceReencode(t *testing.T) {
	info := MediaInfo{Path: "a.mkv", VideoCodec: "av1"}
	if !info.IsMatroskaAV1Input() {
		t.Fatalf("test fixture should be detected as matroska av1")
	}
	if shouldSkipAlreadyEncoded(info, EncodeOptions{}) != true {
		t.Fatalf("matroska av1 should skip by default")
	}
	if shouldSkipAlreadyEncoded(info, EncodeOptions{ForceReencode: true}) {
		t.Fatalf("force reencode should bypass matroska av1 skip")
	}
}

func TestNoVideoStreamErrorIsTyped(t *testing.T) {
	err := fmt.Errorf("image.png: %w", errNoVideoStream)
	if !errors.Is(err, errNoVideoStream) {
		t.Fatalf("no-video error should be detectable")
	}
}

func TestNotVideoFileErrorIsTyped(t *testing.T) {
	err := fmt.Errorf("image.png: %w", errNotVideoFile)
	if !errors.Is(err, errNotVideoFile) {
		t.Fatalf("not-video error should be detectable")
	}
}

func TestEncodeFFmpegRequirements(t *testing.T) {
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	req := encodeFFmpegRequirements(opts)
	if !req.SVTAV1 || !req.VMAF {
		t.Fatalf("probing encode requirements = %+v, want svt-av1 and vmaf", req)
	}
	opts.CRFSet = true
	req = encodeFFmpegRequirements(opts)
	if !req.SVTAV1 || req.VMAF {
		t.Fatalf("explicit CRF encode requirements = %+v, want svt-av1 only", req)
	}
	opts.DryRun = true
	req = encodeFFmpegRequirements(opts)
	if req.SVTAV1 || req.VMAF {
		t.Fatalf("explicit CRF dry-run requirements = %+v, want none", req)
	}
}

func TestFFmpegListHasName(t *testing.T) {
	out := `
 V..... libsvtav1            SVT-AV1(Scalable Video Technology for AV1) encoder
 ... libvmaf          VV->V      Calculate the VMAF between two video streams.
`
	if !ffmpegListHasName(out, "libsvtav1") {
		t.Fatalf("missing libsvtav1")
	}
	if !ffmpegListHasName(out, "libvmaf") {
		t.Fatalf("missing libvmaf")
	}
	if ffmpegListHasName(out, "vmaf") {
		t.Fatalf("matched partial filter name")
	}
}

func TestEncodeHelpDoesNotMentionEnvironment(t *testing.T) {
	var buf bytes.Buffer
	printEncodeHelp(&buf)
	text := buf.String()
	for _, unwanted := range []string{"Environment:", "REENCODE_PRESET", "FORCE_CRF", "FALLBACK_CRF"} {
		if contains(text, unwanted) {
			t.Fatalf("help still contains %q:\n%s", unwanted, text)
		}
	}
	for _, want := range []string{"--crf float", "--fallback-crf float", "Default: 4"} {
		if !contains(text, want) {
			t.Fatalf("help missing %q:\n%s", want, text)
		}
	}
}

func TestCRFQConversions(t *testing.T) {
	for _, crf := range []float64{5, 27, 33.5, 37.25, 70} {
		q := qFromCRF(crf)
		got := crfFromQ(q)
		if math.Abs(got-crf) > 0.000001 {
			t.Fatalf("crf roundtrip %v -> %v -> %v", crf, q, got)
		}
	}
}

func TestVMAFTargets(t *testing.T) {
	got := vmafTargets(95, 94)
	want := []float64{95, 94.8, 94.6, 94.4, 94.2, 94}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.000001 {
			t.Fatalf("target[%d] = %v, want %v; all %v", i, got[i], want[i], got)
		}
	}
}

func TestInterpolateQUsesWorstSampleFloor(t *testing.T) {
	search := crfSearch{
		options: ProbeOptions{
			FloorVMAF:         94,
			MaxEncodedPercent: 90,
		},
		attempts: map[int]ProbeAttempt{},
	}
	for _, attempt := range []ProbeAttempt{
		{CRF: 21, Score: 96.39, WorstSampleScore: 95.57, EncodedPercent: 2},
		{CRF: 29.25, Score: 95.59, WorstSampleScore: 93.84, EncodedPercent: 2},
		{CRF: 37.5, Score: 93.99, WorstSampleScore: 90.25, EncodedPercent: 1},
	} {
		search.attempts[qFromCRF(attempt.CRF)] = attempt
	}

	got := search.interpolateQ(95, qFromCRF(29.25), qFromCRF(21), qFromCRF(37.5))
	if got >= qFromCRF(29.25) {
		t.Fatalf("interpolated CRF = %s, want below known floor failure 29.25", terseFloat(crfFromQ(got)))
	}
}

func TestPlanSamples(t *testing.T) {
	info := MediaInfo{Path: "a.mp4", Duration: 5 * time.Minute, FPS: 30, VideoIndex: 0, VideoCodec: "h264"}
	plan := planSamples(info, 0, 20*time.Second)
	if plan.FullPass {
		t.Fatalf("unexpected full pass")
	}
	if plan.Count != 4 {
		t.Fatalf("count = %d, want 4", plan.Count)
	}
	if plan.Frames != 600 {
		t.Fatalf("frames = %d, want 600", plan.Frames)
	}
	if len(plan.StartsSeconds) != 4 {
		t.Fatalf("starts = %v", plan.StartsSeconds)
	}
	if plan.StartsSeconds[0] <= 0 || plan.StartsSeconds[len(plan.StartsSeconds)-1] >= info.Duration.Seconds()-plan.SampleSeconds {
		t.Fatalf("bad starts: %v", plan.StartsSeconds)
	}
}

func TestPlanSamplesVeryShortFullPass(t *testing.T) {
	info := MediaInfo{Path: "a.mp4", Duration: 30 * time.Second, FPS: 24, VideoIndex: 0, VideoCodec: "h264"}
	plan := planSamples(info, 0, 20*time.Second)
	if !plan.FullPass {
		t.Fatalf("expected full pass: %+v", plan)
	}
	if plan.Count != 1 {
		t.Fatalf("count = %d, want 1", plan.Count)
	}
}

func TestCreateSamplesUsesSourceWindows(t *testing.T) {
	info := MediaInfo{Path: "a.mp4", Duration: 5 * time.Minute, FPS: 30, VideoBytes: 3000, VideoIndex: 0, VideoCodec: "h264"}
	plan := SamplePlan{
		Count:          2,
		SampleDuration: 10 * time.Second,
		SampleSeconds:  10,
		StartsSeconds:  []float64{30, 90},
		Frames:         300,
	}
	samples, err := createSamples(info, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if samples[0].SourcePath != info.Path || samples[0].Start != 30*time.Second || samples[0].Frames != 300 {
		t.Fatalf("bad first sample: %+v", samples[0])
	}
	if samples[0].SourceBytes != 100 {
		t.Fatalf("source bytes = %d, want 100", samples[0].SourceBytes)
	}
}

func TestParseStreamByteTags(t *testing.T) {
	tags := map[string]string{"NUMBER_OF_BYTES-eng": "5576973139"}
	if got := parseStreamByteTags(tags); got != 5576973139 {
		t.Fatalf("bytes = %d", got)
	}
}

func TestEstimateWindowBytes(t *testing.T) {
	got := estimateWindowBytes(3000, 5*time.Minute, 10*time.Second)
	if got != 100 {
		t.Fatalf("window bytes = %d, want 100", got)
	}
}

func TestFFmpegWaitErrorOmitsStderrOnInterrupt(t *testing.T) {
	err := ffmpegWaitError("encode", errors.New("signal: killed"), "ffmpeg stderr dump", context.Canceled)
	text := err.Error()
	if !contains(text, "interrupted") {
		t.Fatalf("interrupt error missing interrupted text: %q", text)
	}
	if contains(text, "ffmpeg stderr dump") {
		t.Fatalf("interrupt error should not include ffmpeg stderr: %q", text)
	}
}

func TestFFmpegFailedErrorIncludesStderrForRealFailure(t *testing.T) {
	err := ffmpegWaitError("encode", errors.New("exit status 1"), "ffmpeg stderr dump", nil)
	text := err.Error()
	if !contains(text, "ffmpeg failed") || !contains(text, "ffmpeg stderr dump") {
		t.Fatalf("failure error should include stderr: %q", text)
	}
}

func TestProgressFrameParser(t *testing.T) {
	frame, ok := parseProgressFrame("frame=123")
	if !ok || frame != 123 {
		t.Fatalf("frame parse = %d %v", frame, ok)
	}
	if _, ok := parseProgressFrame("out_time_ms=123"); ok {
		t.Fatalf("out_time_ms should not count as frame progress")
	}
}

func TestProgressParserMediaTimeAndSpeed(t *testing.T) {
	update, ok := parseFFmpegProgressLine("out_time_ms=2500000")
	if !ok || !update.HaveOutTime || update.OutTime != 2500*time.Millisecond {
		t.Fatalf("out_time_ms parse = %+v %v", update, ok)
	}
	update, ok = parseFFmpegProgressLine("speed=1.25x")
	if !ok || !update.HaveSpeed || math.Abs(update.Speed-1.25) > 0.000001 {
		t.Fatalf("speed parse = %+v %v", update, ok)
	}
}

func TestProgressRatioClamps(t *testing.T) {
	if got := progressRatio(5*time.Second, 10*time.Second); got != 0.5 {
		t.Fatalf("ratio = %v, want 0.5", got)
	}
	if got := progressRatio(11*time.Second, 10*time.Second); got != 1 {
		t.Fatalf("ratio = %v, want 1", got)
	}
	if got := progressRatio(time.Second, 0); got != 0 {
		t.Fatalf("ratio = %v, want 0", got)
	}
}

func TestProgressRatioFramesClamps(t *testing.T) {
	if got := progressRatioFrames(5, 10); got != 0.5 {
		t.Fatalf("ratio = %v, want 0.5", got)
	}
	if got := progressRatioFrames(11, 10); got != 1 {
		t.Fatalf("ratio = %v, want 1", got)
	}
	if got := progressRatioFrames(1, 0); got != 0 {
		t.Fatalf("ratio = %v, want 0", got)
	}
}

func TestProgressLineIncludesMediaTime(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:      "final encode",
		File:      "/tmp/some-long-file-name.mkv",
		CRF:       32,
		Frame:     120,
		MediaTime: 5 * time.Second,
		Expected:  10 * time.Second,
		Speed:     1.5,
		HaveSpeed: true,
	}, 120)
	for _, want := range []string{"final encode crf 32", "50%", "0:05/0:10", "1.50x", "f=120"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if !contains(line, "some-long-file-name.mkv") || contains(line, "/tmp/") {
		t.Fatalf("progress should show basename only: %q", line)
	}
}

func TestProgressLineIncludesProbeScope(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:           "sample vmaf",
		File:           "/tmp/sample.mkv",
		CRF:            32,
		ScopeLabel:     "probe crf 32",
		ScopeDone:      3,
		ScopeTotal:     8,
		Frame:          120,
		ExpectedFrames: 240,
	}, 120)
	for _, want := range []string{"probe crf 32", "37%", "3/8", "| sample vmaf crf 32", "120/240f"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if contains(line, " ? ") {
		t.Fatalf("progress should not show unknown speed placeholder: %q", line)
	}
}

func TestProgressLineFallsBackToFrames(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:           "sample vmaf",
		File:           "/tmp/sample.mkv",
		CRF:            32,
		Frame:          120,
		ExpectedFrames: 240,
		Expected:       10 * time.Second,
	}, 120)
	for _, want := range []string{"sample vmaf crf 32", "50%", "120/240f", "f=120"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if contains(line, " ? ") {
		t.Fatalf("progress should not show unknown speed placeholder: %q", line)
	}
}

func TestProgressLineUsesMediaTimeBeforeFrames(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:           "sample vmaf",
		File:           "/tmp/sample.mkv",
		Frame:          20,
		ExpectedFrames: 100,
		MediaTime:      2 * time.Second,
		Expected:       10 * time.Second,
	}, 120)
	for _, want := range []string{"20%", "0:02/0:10"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if contains(line, "20/100f") {
		t.Fatalf("media time should be preferred over frame text: %q", line)
	}
}

func TestProgressLineUsesFramesWhenMediaTimeLags(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:           "sample vmaf",
		File:           "/tmp/sample.mkv",
		Frame:          120,
		ExpectedFrames: 240,
		MediaTime:      2 * time.Second,
		Expected:       10 * time.Second,
	}, 120)
	for _, want := range []string{"50%", "120/240f"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestProgressLineIndeterminateWithoutTotals(t *testing.T) {
	line := formatProgressLine(ProgressState{
		Role:  "sample vmaf",
		File:  "/tmp/sample.mkv",
		Frame: 120,
	}, 120)
	for _, want := range []string{"?%", "?/?", "f=120"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestDisplayPathUsesBaseName(t *testing.T) {
	if got := displayPath("/tmp/nested/file.mkv"); got != "file.mkv" {
		t.Fatalf("displayPath = %q, want file.mkv", got)
	}
	if got := displayPath("file.mkv"); got != "file.mkv" {
		t.Fatalf("displayPath basename = %q, want file.mkv", got)
	}
}

func TestProgressDisplayPrintLineClearsLiveLine(t *testing.T) {
	var buf bytes.Buffer
	p := &ProgressDisplay{out: &buf, live: true, renderedLines: 1}
	p.PrintLine(">>> crf 32  VMAF 95.00")
	got := buf.String()
	for _, want := range []string{"\x1b[1F", "\x1b[2K", ">>> crf 32  VMAF 95.00\n"} {
		if !contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if p.renderedLines != 0 {
		t.Fatalf("renderedLines = %d, want 0", p.renderedLines)
	}
}

func TestFormatProbeAttemptLine(t *testing.T) {
	line := formatProbeAttemptLine(ProbeAttempt{
		CRF:              32.25,
		Score:            95.123,
		WorstSampleScore: 94.456,
		EncodedPercent:   72.4,
		PredictedSize:    1500,
	})
	for _, want := range []string{"crf 32.25", "VMAF  95.12", "worst  94.46", "size   72%", "predicted 1.5 KiB"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestFormatSelectedProbeAttemptLine(t *testing.T) {
	line := formatSelectedProbeAttemptLine(ProbeAttempt{
		CRF:              43.5,
		Score:            96.18,
		WorstSampleScore: 94.07,
		EncodedPercent:   80,
		PredictedSize:    241 * 1024 * 1024,
	})
	for _, want := range []string{">>> selected crf  43.5", "VMAF  96.18", "worst  94.07", "size   80%", "predicted 241.0 MiB"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestPrintProbeHumanSaysEncodeWouldUseCRF(t *testing.T) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printProbeHuman(ProbeResult{
		File:             "/tmp/input/a.mkv",
		Success:          true,
		CRF:              24.25,
		Score:            96.57,
		WorstSampleScore: 94.02,
		EncodedPercent:   16,
		PredictedSize:    8624,
	})
	_ = w.Close()
	os.Stdout = old
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{"a.mkv: encode would use crf 24.25", "VMAF  96.57", "worst  94.02", "size   16%"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if contains(line, "/tmp/input/") {
		t.Fatalf("human probe output should show basename only: %q", line)
	}
	if contains(line, "time ") {
		t.Fatalf("line should not include predicted time: %q", line)
	}
}

func TestPrintProbeHumanExplainsAcceptedLocalLowSample(t *testing.T) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	printProbeHuman(ProbeResult{
		File:             "a.mkv",
		Success:          true,
		CRF:              24.25,
		Score:            95.2,
		WorstSampleScore: 93.9,
		EncodedPercent:   16,
		PredictedSize:    8624,
		OutlierAccepted:  true,
		OutlierScore:     93.9,
	})
	_ = w.Close()
	os.Stdout = old
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !contains(text, "accepted one local low sample 93.90") {
		t.Fatalf("missing accepted local low sample explanation:\n%s", text)
	}
}

func TestFormatProbeAttemptLineDoesNotInlineAcceptedOutlier(t *testing.T) {
	line := formatProbeAttemptLine(ProbeAttempt{
		CRF:              24.5,
		Score:            95.1,
		WorstSampleScore: 93.9,
		EncodedPercent:   70,
		PredictedSize:    1500,
		OutlierAccepted:  true,
		OutlierScore:     93.9,
	})
	if contains(line, "local low sample") {
		t.Fatalf("attempt line should not inline outlier confirmation: %q", line)
	}
}

func TestFormatOutlierAcceptedLine(t *testing.T) {
	line := formatOutlierAcceptedLine(ProbeAttempt{OutlierScore: 93.9})
	for _, want := range []string{"accepted one local low sample 93.90", "nearby windows passed the VMAF floor"} {
		if !contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestPrintOutlierAcceptedProgress(t *testing.T) {
	var buf bytes.Buffer
	progress := &ProgressDisplay{out: &buf}
	search := crfSearch{options: ProbeOptions{Progress: progress}}
	search.printOutlierAcceptedProgress(qFromCRF(24.5), ProbeAttempt{OutlierAccepted: true, OutlierScore: 93.9})
	search.printOutlierAcceptedProgress(qFromCRF(24.5), ProbeAttempt{OutlierAccepted: true, OutlierScore: 93.9})
	text := buf.String()
	if !contains(text, "accepted one local low sample 93.90") {
		t.Fatalf("accepted outlier progress not printed:\n%s", text)
	}
	if countOccurrences(text, "accepted one local low sample") != 1 {
		t.Fatalf("accepted outlier progress should print once:\n%s", text)
	}
}

func TestProbeAttemptOutlierJSON(t *testing.T) {
	data, err := json.Marshal(ProbeAttempt{
		CRF:                   24.5,
		OutlierChecked:        true,
		OutlierAccepted:       true,
		OutlierScore:          93.9,
		OutlierNeighborScores: []float64{94.2, 94.4},
		sampleScores:          []float64{93.9},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"outlier_checked", "outlier_accepted", "outlier_score", "outlier_neighbor_scores"} {
		if !contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	if contains(text, "sampleScores") {
		t.Fatalf("internal sample scores leaked into JSON: %s", text)
	}
}

func TestSingleBorderlineOutlierAcceptsOneMildMiss(t *testing.T) {
	search := crfSearch{
		options: ProbeOptions{FloorVMAF: 94},
		samples: []SampleFile{
			{Duration: 10 * time.Second},
			{Duration: 10 * time.Second},
			{Duration: 10 * time.Second},
		},
	}
	idx, ok := search.singleBorderlineOutlier(ProbeAttempt{sampleScores: []float64{94.2, 93.9, 94.4}})
	if !ok || idx != 1 {
		t.Fatalf("outlier = %d %v, want 1 true", idx, ok)
	}
}

func TestSingleBorderlineOutlierRejectsSevereMiss(t *testing.T) {
	search := crfSearch{
		options: ProbeOptions{FloorVMAF: 94},
		samples: []SampleFile{
			{Duration: 10 * time.Second},
			{Duration: 10 * time.Second},
		},
	}
	if _, ok := search.singleBorderlineOutlier(ProbeAttempt{sampleScores: []float64{93.1, 94.4}}); ok {
		t.Fatalf("severe miss should not be a borderline outlier")
	}
}

func TestSingleBorderlineOutlierRejectsMultipleMisses(t *testing.T) {
	search := crfSearch{
		options: ProbeOptions{FloorVMAF: 94},
		samples: []SampleFile{
			{Duration: 10 * time.Second},
			{Duration: 10 * time.Second},
			{Duration: 10 * time.Second},
		},
	}
	if _, ok := search.singleBorderlineOutlier(ProbeAttempt{sampleScores: []float64{93.9, 94.4, 93.8}}); ok {
		t.Fatalf("multiple misses should not be a single outlier")
	}
}

func TestOutlierNeighborSamplesClampAndDeduplicate(t *testing.T) {
	search := crfSearch{info: MediaInfo{Duration: 60 * time.Second}}
	sample := SampleFile{SourcePath: "a.mkv", Start: 5 * time.Second, Duration: 20 * time.Second, Frames: 600}
	neighbors := search.outlierNeighborSamples(sample)
	if len(neighbors) != 2 {
		t.Fatalf("neighbors = %d, want 2: %+v", len(neighbors), neighbors)
	}
	if neighbors[0].Start != 0 || neighbors[1].Start != 15*time.Second {
		t.Fatalf("bad neighbor starts: %+v", neighbors)
	}
}

func TestOutlierNeighborScoresMustAllPass(t *testing.T) {
	if !allScoresAtLeast([]float64{94, 94.2}, 94) {
		t.Fatalf("passing neighbors should pass")
	}
	if allScoresAtLeast([]float64{94.2, 93.99}, 94) {
		t.Fatalf("one failing neighbor should reject outlier")
	}
}

func TestBuildVideoArgsLongInput(t *testing.T) {
	info := MediaInfo{Duration: 5 * time.Minute, FPS: 24, VideoIndex: 0, VideoCodec: "h264"}
	video := buildVideoArgs(info, "4", 32.25)
	args := video.ffmpegArgs("-c:v")
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	for _, want := range []string{"-c:v libsvtav1", "-preset 4", "-crf 32.25", "-pix_fmt yuv420p10le", "-g 240", "-svtav1-params scd=1:crf=32.25"} {
		if !contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestBuildVideoArgsClampSvtFFmpegCRFOnly(t *testing.T) {
	info := MediaInfo{Duration: time.Minute, FPS: 24, VideoIndex: 0, VideoCodec: "h264"}
	video := buildVideoArgs(info, "4", 66)
	args := video.ffmpegArgs("-c:v")
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	if !contains(joined, "-crf 63") {
		t.Fatalf("missing clamped ffmpeg crf in %q", joined)
	}
	if !contains(joined, "-svtav1-params scd=0:crf=66") {
		t.Fatalf("missing real svt crf in %q", joined)
	}
}

func TestSampleEncodeArgsUseSourceWindow(t *testing.T) {
	sample := SampleFile{SourcePath: "source.mkv", Start: 30 * time.Second, Frames: 300}
	video := VideoArgs{Codec: "libsvtav1", Preset: "4", CRF: 25.75, PixFmt: "yuv420p10le"}
	args := sampleEncodeArgs(video, sample, "encoded.mkv")
	joined := joinArgs(args)
	for _, want := range []string{"-ss 30", "-i source.mkv", "-frames:v 300", "-map 0:v:0", "-c:v libsvtav1", "-crf 25.75", "-an -sn -dn encoded.mkv"} {
		if !contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	if contains(joined, "-c:v copy") {
		t.Fatalf("sample encode should not stream-copy reference samples: %q", joined)
	}
}

func TestSampleScoreArgsSeekReferenceSource(t *testing.T) {
	info := MediaInfo{Width: 1920, Height: 1080}
	sample := SampleFile{SourcePath: "source.mkv", Start: 30 * time.Second, Frames: 300}
	args := sampleScoreArgs(info, sample, "encoded.mkv")
	joined := joinArgs(args)
	for _, want := range []string{"-i encoded.mkv", "-ss 30", "-i source.mkv", "-filter_complex", "-frames:v 300", "libvmaf="} {
		if !contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestStreamMapArgsDropsData(t *testing.T) {
	info := MediaInfo{Streams: []StreamInfo{
		{Index: 0, Type: "video"},
		{Index: 1, Type: "audio"},
		{Index: 2, Type: "subtitle"},
		{Index: 3, Type: "attachment"},
		{Index: 4, Type: "data"},
	}}
	args, dropped := streamMapArgs(info)
	if len(dropped) != 1 || dropped[0].Index != 4 {
		t.Fatalf("dropped = %+v", dropped)
	}
	if len(args) != 8 {
		t.Fatalf("args = %v", args)
	}
}

func TestVMAFFilterDefault1080p(t *testing.T) {
	info := MediaInfo{Width: 1920, Height: 1080}
	got := vmafFilter(info)
	for _, want := range []string{
		"[0:v]format=yuv420p10le,setpts=PTS-STARTPTS,settb=AVTB[dis];",
		"[1:v]format=yuv420p10le,setpts=PTS-STARTPTS,settb=AVTB[ref];",
		"[dis][ref]libvmaf=shortest=true:ts_sync_mode=nearest:pool=harmonic_mean:",
	} {
		if !contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if contains(got, "scale=") {
		t.Fatalf("unexpected scale in %q", got)
	}
	if contains(got, "vmaf_4k") {
		t.Fatalf("unexpected 4k model in %q", got)
	}
}

func TestVMAFFilterScalesLowResolution(t *testing.T) {
	info := MediaInfo{Width: 1280, Height: 720}
	got := vmafFilter(info)
	if !contains(got, "scale=1920:-1:flags=bicubic") {
		t.Fatalf("expected 1080p-oriented scale in %q", got)
	}
}

func TestVMAFFilterUses4KModel(t *testing.T) {
	info := MediaInfo{Width: 3840, Height: 2160}
	got := vmafFilter(info)
	if !contains(got, "model=version=vmaf_4k_v0.6.1") {
		t.Fatalf("expected 4k model in %q", got)
	}
	if contains(got, "scale=") {
		t.Fatalf("unexpected scale in native 4k filter %q", got)
	}
}

func TestVMAFFilterScalesToward4K(t *testing.T) {
	info := MediaInfo{Width: 3008, Height: 1692}
	got := vmafFilter(info)
	if !contains(got, "scale=3840:-1:flags=bicubic") {
		t.Fatalf("expected 4k-oriented scale in %q", got)
	}
	if !contains(got, "model=version=vmaf_4k_v0.6.1") {
		t.Fatalf("expected 4k model in %q", got)
	}
}

func TestSummarizeScoresUsesDurationWeightedMeanAndWorst(t *testing.T) {
	got := summarizeScores([]sampleScore{
		{Score: 96, Duration: 10 * time.Second},
		{Score: 90, Duration: 30 * time.Second},
	})
	if math.Abs(got.Mean-91.5) > 0.000001 {
		t.Fatalf("mean = %v, want 91.5", got.Mean)
	}
	if got.Worst != 90 {
		t.Fatalf("worst = %v, want 90", got.Worst)
	}
}

func TestProbeAttemptWorstSampleGuardrail(t *testing.T) {
	opts := defaultProbeOptions()
	opts.FloorVMAF = 94
	search := crfSearch{options: opts}
	attempt := ProbeAttempt{Score: 95.2, WorstSampleScore: 93.9, EncodedPercent: 50}
	if attempt.Score >= 95 && attempt.WorstSampleScore >= search.options.FloorVMAF && attempt.EncodedPercent <= search.options.MaxEncodedPercent {
		t.Fatalf("guardrail unexpectedly accepted attempt: %+v", attempt)
	}
}

func TestInitialGroupCRFUsesMostConservativeCRF(t *testing.T) {
	inputs := []groupInput{
		{Result: ProbeResult{CRF: 32}},
		{Result: ProbeResult{CRF: 30}},
		{Result: ProbeResult{CRF: 34}},
	}
	got := initialGroupCRF(inputs)
	if got != 30 {
		t.Fatalf("initialGroupCRF = %v, want 30", got)
	}
}

func TestSeedProbeSessionAttempts(t *testing.T) {
	session := &probeSession{search: crfSearch{attempts: map[int]ProbeAttempt{}}}
	result := ProbeResult{
		File:             "cached.mkv",
		Success:          true,
		CRF:              24.25,
		Score:            95.2,
		WorstSampleScore: 94.1,
		EncodedPercent:   50,
		PredictedSize:    1234,
		Attempts: []ProbeAttempt{
			{CRF: 25, Score: 94.8, WorstSampleScore: 93.9, EncodedPercent: 40},
		},
	}
	seedProbeSessionAttempts(session, result)

	if session.result.File != "cached.mkv" {
		t.Fatalf("session result not seeded: %+v", session.result)
	}
	if _, ok := session.search.attempts[qFromCRF(25)]; !ok {
		t.Fatalf("cached attempt was not seeded")
	}
	selected, ok := session.search.attempts[qFromCRF(24.25)]
	if !ok {
		t.Fatalf("selected result attempt was not seeded")
	}
	if selected.Score != 95.2 || selected.WorstSampleScore != 94.1 || selected.PredictedSize != 1234 {
		t.Fatalf("bad selected attempt: %+v", selected)
	}
}

func TestChooseGroupCRFUsesSeededAttempts(t *testing.T) {
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	opts.ProbeOptions.TargetVMAF = 95
	opts.ProbeOptions.FloorVMAF = 94
	inputs := []groupInput{
		{
			Result: ProbeResult{Success: true, CRF: 30},
			Session: &probeSession{search: crfSearch{
				options: opts.ProbeOptions,
				attempts: map[int]ProbeAttempt{
					qFromCRF(30): {CRF: 30, Score: 95.2, WorstSampleScore: 94.2, EncodedPercent: 50},
				},
			}},
		},
		{
			Result: ProbeResult{Success: true, CRF: 31},
			Session: &probeSession{search: crfSearch{
				options: opts.ProbeOptions,
				attempts: map[int]ProbeAttempt{
					qFromCRF(30): {CRF: 30, Score: 95.4, WorstSampleScore: 94.5, EncodedPercent: 60},
				},
			}},
		},
	}

	crf, warnings, err := chooseGroupCRF(context.Background(), opts, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if crf != 30 {
		t.Fatalf("group CRF = %v, want 30", crf)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
}

func TestEvaluateCRFPrintsCachedAcceptedLocalLowSample(t *testing.T) {
	var buf bytes.Buffer
	opts := defaultProbeOptions()
	opts.TargetVMAF = 95
	opts.Progress = &ProgressDisplay{out: &buf}
	session := &probeSession{search: crfSearch{
		options:  opts,
		attempts: map[int]ProbeAttempt{},
	}}
	attempt := ProbeAttempt{
		CRF:              24.5,
		Score:            95.2,
		WorstSampleScore: 93.9,
		EncodedPercent:   50,
		OutlierAccepted:  true,
		OutlierScore:     93.9,
	}
	session.search.attempts[qFromCRF(24.5)] = attempt

	if _, err := session.EvaluateCRF(context.Background(), 24.5); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !contains(text, "accepted one local low sample 93.90") {
		t.Fatalf("missing cached accepted local low sample explanation:\n%s", text)
	}
}

func TestPersistGroupProbeCachesWritesExpandedAttempts(t *testing.T) {
	dir := t.TempDir()
	opts := defaultProbeOptions()
	cacheOpts := normalizedProbeCacheOptions(opts)
	key, err := probeCacheOptionsKey(cacheOpts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	handle := newProbeCacheHandle(dir, "binary", fingerprint, fingerprintKey, cacheOpts, key, "a.mkv")
	inputs := []groupInput{{
		File:   "a.mkv",
		Cache:  handle,
		Result: ProbeResult{File: "a.mkv", Success: true, CRF: 24.25},
		Session: &probeSession{search: crfSearch{attempts: map[int]ProbeAttempt{
			qFromCRF(24.25): {CRF: 24.25, Score: 95.2, WorstSampleScore: 94.1},
			qFromCRF(24):    {CRF: 24, Score: 95.4, WorstSampleScore: 94.3},
		}}},
	}}

	persistGroupProbeCaches(context.Background(), opts, inputs)
	got, ok, err := loadProbeCache(handle, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want two expanded attempts", got.Attempts)
	}
}

func TestGroupFallbackPersistsExpandedAttempts(t *testing.T) {
	dir := t.TempDir()
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	cacheOpts := normalizedProbeCacheOptions(opts.ProbeOptions)
	key, err := probeCacheOptionsKey(cacheOpts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	handle := newProbeCacheHandle(dir, "binary", fingerprint, fingerprintKey, cacheOpts, key, "a.mkv")
	inputs := []groupInput{{
		File:   "a.mkv",
		Cache:  handle,
		Result: ProbeResult{File: "a.mkv", Success: true, CRF: 24.25},
		Session: &probeSession{search: crfSearch{attempts: map[int]ProbeAttempt{
			qFromCRF(24.25): {CRF: 24.25, Score: 95.2, WorstSampleScore: 94.1},
			qFromCRF(23.75): {CRF: 23.75, Score: 95.5, WorstSampleScore: 94.4},
		}}},
	}}

	err = groupFallbackOrError(context.Background(), opts, inputs, errors.New("no shared CRF"))
	if err == nil {
		t.Fatalf("expected group fallback error")
	}
	got, ok, err := loadProbeCache(handle, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want persisted expanded attempts", got.Attempts)
	}
}

func TestSingleFileFallbackPersistsExpandedAttempts(t *testing.T) {
	dir := t.TempDir()
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	cacheOpts := normalizedProbeCacheOptions(opts.ProbeOptions)
	key, err := probeCacheOptionsKey(cacheOpts)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := inputFingerprint{Size: 123, SampleHash: "sample"}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	handle := newProbeCacheHandle(dir, "binary", fingerprint, fingerprintKey, cacheOpts, key, "a.mkv")
	result := ProbeResult{File: "a.mkv", Success: true, CRF: 24.25}
	session := &probeSession{search: crfSearch{attempts: map[int]ProbeAttempt{
		qFromCRF(24.25): {CRF: 24.25, Score: 95.2, WorstSampleScore: 94.1},
		qFromCRF(24):    {CRF: 24, Score: 95.4, WorstSampleScore: 94.3},
	}}}

	persistProbeSessionCache(context.Background(), opts.ProbeOptions, handle, session, result)
	got, ok, err := loadProbeCache(handle, "a.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected cache hit")
	}
	if len(got.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want persisted expanded attempts", got.Attempts)
	}
}

func TestGroupQualityOKIgnoresSizeCap(t *testing.T) {
	opts := defaultProbeOptions()
	opts.TargetVMAF = 95
	opts.FloorVMAF = 94
	opts.MaxEncodedPercent = 90
	attempt := ProbeAttempt{Score: 95.1, WorstSampleScore: 94.2, EncodedPercent: 140}
	if !groupQualityOK(attempt, opts) {
		t.Fatalf("quality should pass even when size cap fails: %+v", attempt)
	}
}

func TestGroupQualityOKRequiresWorstSampleFloor(t *testing.T) {
	opts := defaultProbeOptions()
	opts.TargetVMAF = 95
	opts.FloorVMAF = 94
	attempt := ProbeAttempt{Score: 95.1, WorstSampleScore: 93.9, EncodedPercent: 50}
	if groupQualityOK(attempt, opts) {
		t.Fatalf("quality should fail on worst sample floor: %+v", attempt)
	}
}

func TestGroupQualityOKAcceptsConfirmedOutlier(t *testing.T) {
	opts := defaultProbeOptions()
	opts.TargetVMAF = 95
	opts.FloorVMAF = 94
	attempt := ProbeAttempt{Score: 95.1, WorstSampleScore: 93.9, OutlierAccepted: true}
	if !groupQualityOK(attempt, opts) {
		t.Fatalf("confirmed outlier should pass quality: %+v", attempt)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func countOccurrences(s, sub string) int {
	count := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			count++
			i += len(sub) - 1
		}
	}
	return count
}

func joinArgs(args []string) string {
	joined := ""
	for _, arg := range args {
		joined += arg + " "
	}
	return joined
}
