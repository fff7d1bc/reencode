package main

import (
	"context"
	"crypto/sha512"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed models/*.json models/LICENSE
var vmafAssets embed.FS

type VMAFModelInfo struct {
	Profile          string `json:"profile"`
	ID               string `json:"id"`
	SHA512           string `json:"sha512,omitempty"`
	Scale            string `json:"scoring_scale,omitempty"`
	PixelFormat      string `json:"pixel_format"`
	Pool             string `json:"pool"`
	EncodingWidth    int    `json:"encoding_width"`
	EncodingHeight   int    `json:"encoding_height"`
	EncodingBitDepth int    `json:"encoding_bit_depth"`
	Policy           int    `json:"scoring_policy"`
}

type RuntimeVersions struct {
	LibVMAF string `json:"libvmaf"`
	SVTAV1  string `json:"svt_av1"`
}

func resolveVMAFModel(opts ProbeOptions, info MediaInfo) VMAFModelInfo {
	m := VMAFModelInfo{Profile: opts.VMAFProfile, PixelFormat: defaultVMAFPixFmt, Pool: "harmonic_mean", EncodingWidth: info.Width, EncodingHeight: info.Height, EncodingBitDepth: 10, Policy: 2}
	if m.Profile == "legacy" {
		m.ID = vmafModelVersion(info)
		m.Scale = vmafScaleFilter(info.Width, info.Height)
		return m
	}
	m.Profile = "v1"
	m.ID = "vmaf_v1.0.16"
	if info.FPS >= 45 {
		m.ID += "_hfr"
	}
	long, short := max(info.Width, info.Height), min(info.Width, info.Height)
	if vmafUse4KModel(long, short) {
		m.ID += "_1d5h_2160"
	} else {
		m.ID += "_3d0h"
	}
	m.Scale = vmafScaleFilter(long, short)
	if info.Height > info.Width && m.Scale != "" {
		// Apply the same display heuristic after rotating its target canvas.
		parts := strings.Split(m.Scale, ":")
		parts[0], parts[1] = "scale="+parts[1], strings.TrimPrefix(parts[0], "scale=")
		m.Scale = strings.Join(parts, ":")
	}
	data, err := vmafAssets.ReadFile("models/" + m.ID + ".json")
	if err != nil {
		panic(err)
	} // Every selectable model is embedded at build time.
	m.SHA512 = fmt.Sprintf("%x", sha512.Sum512(data))
	return m
}

func (m VMAFModelInfo) filter(path string) string {
	config := "version=" + m.ID
	if m.Profile == "v1" {
		config = fmt.Sprintf("path=%s:cambi.enc_width=%d:cambi.enc_height=%d:cambi.enc_bitdepth=%d", escapeFFmpegToken(path, ":="), m.EncodingWidth, m.EncodingHeight, m.EncodingBitDepth)
	}
	return configuredVMAFFilter(m.Scale, config)
}

func materializeVMAFModel(m VMAFModelInfo, dir string) (string, error) {
	if m.Profile != "v1" {
		return "", nil
	}
	if dir == "" {
		dir = os.TempDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// FFmpeg splits model lists at literal pipes before parsing escaped options.
	if strings.Contains(dir, "|") {
		return "", fmt.Errorf("VMAF model temp directory cannot contain '|': choose another --temp-dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := vmafAssets.ReadFile("models/" + m.ID + ".json")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".reencode-model-*.json")
	if err != nil {
		return "", err
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("write VMAF model: %v %v", writeErr, closeErr)
	}
	return f.Name(), nil
}

var scoringRuntimeCache = struct {
	sync.Mutex
	versions map[string]RuntimeVersions
}{versions: map[string]RuntimeVersions{}}

var checkScoringRuntime = detectScoringRuntime

func detectScoringRuntime(ctx context.Context, opts ProbeOptions, model VMAFModelInfo) (RuntimeVersions, error) {
	if model.Profile == "v1" && model.EncodingWidth < 216 && model.EncodingHeight < 216 {
		return RuntimeVersions{}, fmt.Errorf("VMAF v1 CAMBI requires at least one encoded dimension >= 216 pixels; use --vmaf-profile legacy for this input")
	}
	encoderVersion, err := checkEncoderTune(ctx, opts)
	if err != nil {
		return RuntimeVersions{}, err
	}
	key := model.ID + "/" + opts.Tune + "/" + opts.Preset
	scoringRuntimeCache.Lock()
	defer scoringRuntimeCache.Unlock()
	if versions, ok := scoringRuntimeCache.versions[key]; ok {
		return versions, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path, err := materializeVMAFModel(model, opts.TempDir)
	if err != nil {
		return RuntimeVersions{}, err
	}
	if path != "" {
		defer os.Remove(path)
	}
	dir := opts.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	logPath, err := reserveTempPath(dir, ".reencode-runtime-*.json")
	if err != nil {
		return RuntimeVersions{}, err
	}
	defer os.Remove(logPath)
	// Exercise the actual dynamically loaded libraries and all selected model
	// features. Upstream v3.2.1 can report "3.2.0" or a git hash, so gate on
	// model support rather than version text. Preserve the reported version for
	// diagnostics and cache identity without inferring a package release.
	smoke := model
	smoke.Scale, smoke.EncodingWidth, smoke.EncodingHeight = "", 640, 360
	filter := strings.ReplaceAll(strings.ReplaceAll(smoke.filter(path), "[0:v]", "[dis0]"), "[1:v]", "[ref0]")
	filter = "[0:v]format=yuv420p10le,split=3[enc][dis0][ref0];" + filter + ":log_fmt=json:log_path=" + escapeFilterOption(logPath) + "[quality]"
	args := []string{"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=0.4", "-filter_complex", filter,
		"-map", "[enc]", "-c:v", "libsvtav1", "-preset", "11", "-crf", "32", "-f", "null", nullOutputPath(),
		"-map", "[quality]", "-f", "null", nullOutputPath()}
	report, err := runVMAFReport(ctx, FFMpegRun{Role: "VMAF runtime check", Args: args, StallTimeout: 15 * time.Second,
		Progress: opts.progressDisplay(), ExpectedFrames: 12, ExpectedDuration: 400 * time.Millisecond}, logPath, 12)
	if err != nil {
		return RuntimeVersions{}, fmt.Errorf("VMAF model %s runtime check failed (upgrade ffmpeg/libvmaf or select --vmaf-profile legacy): %w", model.ID, err)
	}
	if report.SVTVersion == "" {
		return RuntimeVersions{}, fmt.Errorf("cannot determine actual SVT-AV1 runtime version")
	}
	versions := RuntimeVersions{LibVMAF: report.Version, SVTAV1: encoderVersion}
	scoringRuntimeCache.versions[key] = versions
	return versions, nil
}

var svtVersionPattern = regexp.MustCompile(`SVT-AV1 Encoder Lib (v[^\s]+)`)
