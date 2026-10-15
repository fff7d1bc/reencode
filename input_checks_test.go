package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInputCheckOptions(t *testing.T) {
	for _, mode := range []string{"upfront", "per-file"} {
		probe, _, err := parseProbeArgs([]string{"--input-checks", mode, "input"})
		if err != nil || probe.InputChecks != mode {
			t.Fatal(probe, err)
		}
		encode, _, err := parseEncodeArgs([]string{"--input-checks", mode, "input"})
		if err != nil || encode.ProbeOptions.InputChecks != mode {
			t.Fatal(encode, err)
		}
		compare, _, err := parseCompareArgs([]string{"--input-checks", mode, "input"})
		if err != nil || compare.ProbeOptions.InputChecks != mode {
			t.Fatal(compare, err)
		}
	}
	if _, _, err := parseProbeArgs([]string{"--input-checks", "none", "input"}); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	if _, _, err := parseEncodeArgs([]string{"--input-checks", "per-file", "--group-crf", "input"}); err == nil {
		t.Fatal("deferred group accepted")
	}
	if defaultProbeOptions().InputChecks != "upfront" {
		t.Fatal("default changed")
	}
	upfront := defaultProbeOptions()
	perFile := upfront
	perFile.InputChecks = "per-file"
	if normalizedProbeCacheOptions(upfront) != normalizedProbeCacheOptions(perFile) {
		t.Fatal("scheduling split result cache")
	}
	a, _ := measurementKey(upfront, MediaInfo{}, SampleFile{}, VideoArgs{})
	b, _ := measurementKey(perFile, MediaInfo{}, SampleFile{}, VideoArgs{})
	if a != b {
		t.Fatal("scheduling split measurement cache")
	}
}

func TestDeferredChecksLargeBatch(t *testing.T) {
	oldMeta, oldHDR := inspectEligibilityMetadata, inspectEligibilityHDR
	defer func() { inspectEligibilityMetadata, inspectEligibilityHDR = oldMeta, oldHDR }()
	dir := t.TempDir()
	var files []string
	for i := 0; i < 3000; i++ {
		name := fmt.Sprintf("input-%04d", i)
		if i%3 == 0 {
			name += "-skip"
		} else if i%3 == 1 {
			name += "-av1"
		}
		path := filepath.Join(dir, name+".mkv")
		files = append(files, path)
		if i%3 != 0 {
			if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var metadata, decoded atomic.Int32
	inspectEligibilityMetadata = func(_ context.Context, path string, _ bool) (MediaInfo, error) {
		metadata.Add(1)
		info := validationSourceInfo()
		info.Path = path
		if strings.Contains(path, "-av1") {
			info.VideoCodec = "av1"
		}
		return info, nil
	}
	inspectEligibilityHDR = func(context.Context, string) (string, error) { decoded.Add(1); return "", nil }
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), CRFSet: true, CRF: 32, DryRun: true}
	opts.ProbeOptions.InputChecks = "per-file"
	opts.ProbeOptions.SkipNames = []string{"-skip"}
	result := collectEligibleInputs(context.Background(), eligibilityEncode, files, opts)
	if result.Fatal != nil || len(result.Actionable) != 1000 || len(result.Skipped) != 2000 || metadata.Load() != 2000 || decoded.Load() != 0 {
		t.Fatal(len(result.Actionable), len(result.Skipped), metadata.Load(), decoded.Load(), result.Fatal)
	}
	if err := validateEncodePaths(opts, files, result.Actionable); err != nil {
		t.Fatal(err)
	}
	for i, input := range result.Actionable {
		if input.File != files[i*3+2] || input.Info.framesValidated {
			t.Fatal("ordering/readiness changed", i)
		}
	}
	if err := encodeOne(context.Background(), opts, result.Actionable[0]); err != nil {
		t.Fatal(err)
	}
	if decoded.Load() != 1 {
		t.Fatal("decoded files ahead of their turn", decoded.Load())
	}
	// The already-AV1 and name skips remain decode-free in the default mode too.
	opts.ProbeOptions.InputChecks = "upfront"
	result = collectEligibleInputs(context.Background(), eligibilityEncode, files[:3], opts)
	if result.Fatal != nil || decoded.Load() != 2 || !result.Actionable[0].Info.framesValidated {
		t.Fatal(result, decoded.Load())
	}
}

func TestDeferredHDRCannotBypassWithCRFOrCache(t *testing.T) {
	oldMeta, oldHDR := inspectEligibilityMetadata, inspectEligibilityHDR
	defer func() { inspectEligibilityMetadata, inspectEligibilityHDR = oldMeta, oldHDR }()
	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	inspectEligibilityMetadata = func(_ context.Context, path string, _ bool) (MediaInfo, error) {
		info := validationSourceInfo()
		info.Path = path
		return info, nil
	}
	decoded := 0
	inspectEligibilityHDR = func(context.Context, string) (string, error) { decoded++; return "HDR10+", nil }
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), DryRun: true}
	opts.ProbeOptions.InputChecks = "per-file"
	result := collectEligibleInputs(context.Background(), eligibilityEncode, []string{path}, opts)
	if result.Fatal != nil || decoded != 0 {
		t.Fatal(result.Fatal, decoded)
	}
	input := result.Actionable[0]
	for _, variant := range []EncodeOptions{opts, func() EncodeOptions { o := opts; o.CRFSet = true; o.CRF = 32; return o }(), func() EncodeOptions { o := opts; o.FallbackCRFSet = true; o.FallbackCRF = 32; return o }()} {
		if err := encodeOne(context.Background(), variant, input); err == nil || !strings.Contains(err.Error(), "HDR") {
			t.Fatal("unsafe encode", err)
		}
	}
	if _, err := probeInspectedFile(context.Background(), opts.ProbeOptions, input.Info, input.Source); err == nil || !strings.Contains(err.Error(), "HDR") {
		t.Fatal("unsafe probe/cache reuse", err)
	}
	comparison, _ := compareInput(context.Background(), CompareOptions{ProbeOptions: opts.ProbeOptions, Profiles: []string{"legacy", "v1"}, Tunes: []string{"psnr"}}, input.Info)
	if !strings.Contains(comparison.Error, "HDR") || len(comparison.Results) != 0 {
		t.Fatal("unsafe compare", comparison)
	}
	if data, _ := os.ReadFile(path); string(data) != "source" {
		t.Fatal("source modified")
	}
	if _, err := os.Stat(outputPathFor(path)); !os.IsNotExist(err) {
		t.Fatal("output created")
	}
}

func TestDeferredMetadataErrorsAndChangedSource(t *testing.T) {
	oldMeta, oldHDR := inspectEligibilityMetadata, inspectEligibilityHDR
	defer func() { inspectEligibilityMetadata, inspectEligibilityHDR = oldMeta, oldHDR }()
	path := filepath.Join(t.TempDir(), "source.mkv")
	if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	info := validationSourceInfo()
	info.Path = path
	inspectEligibilityMetadata = func(context.Context, string, bool) (MediaInfo, error) { return info, nil }
	decoded := 0
	inspectEligibilityHDR = func(context.Context, string) (string, error) { decoded++; return "", nil }
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions()}
	opts.ProbeOptions.InputChecks = "per-file"
	info.HasAlpha = true
	if result := collectEligibleInputs(context.Background(), eligibilityEncode, []string{path}, opts); result.Fatal == nil || decoded != 0 {
		t.Fatal("metadata alpha not rejected upfront", result)
	}
	info.HasAlpha = false
	info.HDR = "HDR10"
	if result := collectEligibleInputs(context.Background(), eligibilityEncode, []string{path}, opts); result.Fatal == nil || decoded != 0 {
		t.Fatal("metadata HDR not rejected upfront", result)
	}
	info.HDR = ""
	result := collectEligibleInputs(context.Background(), eligibilityEncode, []string{path}, opts)
	if result.Fatal != nil {
		t.Fatal(result.Fatal)
	}
	if err := os.WriteFile(path, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	input := result.Actionable[0]
	if _, err := ensureInputReady(context.Background(), opts.ProbeOptions, input.Info); err == nil || decoded != 0 {
		t.Fatal("changed source inspected/reused", err, decoded)
	}
}

func TestDeferredEncodeContinuesAndChecksConflictsFirst(t *testing.T) {
	oldMeta, oldHDR := inspectEligibilityMetadata, inspectEligibilityHDR
	defer func() { inspectEligibilityMetadata, inspectEligibilityHDR = oldMeta, oldHDR }()
	dir := t.TempDir()
	files := []string{filepath.Join(dir, "first.mkv"), filepath.Join(dir, "second.mkv")}
	for _, path := range files {
		if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inspectEligibilityMetadata = func(_ context.Context, path string, _ bool) (MediaInfo, error) {
		info := validationSourceInfo()
		info.Path = path
		return info, nil
	}
	var decoded []string
	inspectEligibilityHDR = func(_ context.Context, path string) (string, error) {
		decoded = append(decoded, path)
		if path == files[0] {
			return "HDR10+", nil
		}
		return "", nil
	}
	opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), CRFSet: true, CRF: 32, DryRun: true, Overwrite: true}
	opts.ProbeOptions.InputChecks = "per-file"
	if code := runEncodeCommand(context.Background(), opts, files); code != 1 || len(decoded) != 2 || decoded[0] != files[0] || decoded[1] != files[1] {
		t.Fatal(code, decoded)
	}
	for _, path := range files {
		if data, _ := os.ReadFile(path); string(data) != "source" {
			t.Fatal("source changed")
		}
	}
	conflict := outputPathFor(files[0])
	if err := os.WriteFile(conflict, []byte("other input"), 0600); err != nil {
		t.Fatal(err)
	}
	decoded = nil
	if code := runEncodeCommand(context.Background(), opts, []string{files[0], conflict}); code != 1 || len(decoded) != 0 {
		t.Fatal("decoded before conflict check", code, decoded)
	}
}

func TestMediaIntegrationDeferredProbeJSONOrdering(t *testing.T) {
	requireMediaIntegration(t)
	oldMeta, oldHDR, oldRuntime := inspectEligibilityMetadata, inspectEligibilityHDR, checkScoringRuntime
	defer func() {
		inspectEligibilityMetadata, inspectEligibilityHDR, checkScoringRuntime = oldMeta, oldHDR, oldRuntime
	}()
	dir := t.TempDir()
	files := []string{filepath.Join(dir, "first.mkv"), filepath.Join(dir, "second.mkv")}
	for _, path := range files {
		if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	inspectEligibilityMetadata = func(_ context.Context, path string, _ bool) (MediaInfo, error) {
		info := validationSourceInfo()
		info.Path = path
		return info, nil
	}
	inspectEligibilityHDR = func(_ context.Context, path string) (string, error) {
		if path == files[0] {
			return "HDR10+", nil
		}
		return "", nil
	}
	calls := 0
	checkScoringRuntime = func(context.Context, ProbeOptions, VMAFModelInfo) (RuntimeVersions, error) {
		calls++
		return RuntimeVersions{}, fmt.Errorf("runtime stopped")
	}
	out, err := os.CreateTemp(dir, "json-")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldStdout := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = oldStdout }()
	opts := defaultProbeOptions()
	opts.InputChecks = "per-file"
	opts.JSON = true
	code := runProbeCommand(context.Background(), opts, files)
	os.Stdout = oldStdout
	if code != 1 || calls != 1 {
		t.Fatal(code, calls)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(out)
	for i, want := range []string{"HDR", "runtime stopped"} {
		var row ProbeResult
		if err := decoder.Decode(&row); err != nil || row.File != files[i] || !strings.Contains(row.Error, want) {
			t.Fatal(row, err)
		}
	}
}
