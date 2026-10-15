package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCompareOptionsAndIsolation(t *testing.T) {
	opts, files, err := parseCompareArgs([]string{"--json", "--tunes", "vq,psnr", "--samples", "2", "movie"})
	if err != nil || len(files) != 1 || !reflect.DeepEqual(opts.Profiles, []string{"legacy", "v1"}) {
		t.Fatal(opts, files, err)
	}
	for _, args := range [][]string{{"--tune", "vq", "movie"}, {"--vmaf-profiles", "legacy,legacy", "movie"}, {"--tunes", "psnr,", "movie"}, {"--tunes", "nope", "movie"}} {
		if _, _, err := parseCompareArgs(args); err == nil {
			t.Fatal("invalid comparison accepted", args)
		}
	}
	original := compareProbe
	defer func() { compareProbe = original }()
	info := MediaInfo{framesValidated: true, Path: "movie", Duration: 5 * time.Minute, FPS: 24, VideoBytes: 10000, Width: 1920, Height: 1080}
	var calls []string
	compareProbe = func(_ context.Context, o ProbeOptions, i MediaInfo, _ bool) (ProbeResult, *probeSession, *probeCacheHandle, error) {
		if !reflect.DeepEqual(i, info) {
			t.Fatal("input changed")
		}
		s, err := newProbeSession(o, i)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.result.SamplePlan, planSamples(info, 2, 20*time.Second)) {
			t.Fatal("plan changed")
		}
		if len(s.search.attempts) != 0 {
			t.Fatal("search reused")
		}
		calls = append(calls, o.VMAFProfile+"/"+o.Tune)
		if len(calls) == 1 {
			return s.result, s, nil, fmt.Errorf("unsupported")
		}
		s.result.Success = true
		return s.result, s, nil, nil
	}
	result, err := compareInput(context.Background(), opts, info)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"legacy/vq", "legacy/psnr", "v1/vq", "v1/psnr"}) || len(result.Results) != 4 || result.Results[0].Error == "" {
		t.Fatal(calls, result)
	}
	result.Results[1].CacheHit = true
	result.Results[1].Timing = ProbeTiming{EncodeSeconds: 1234}
	var out bytes.Buffer
	printCompareHuman(&out, result)
	if !strings.Contains(out.String(), "cached") || strings.Contains(out.String(), "1234") {
		t.Fatal(out.String())
	}
	if _, err := json.Marshal(result); err != nil {
		t.Fatal(err)
	}
}

func TestMediaIntegrationCompareReadOnlyAndCache(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "compare.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=12", "-t", "1", "-c:v", "mpeg4", "-q:v", "2", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	opts, _, err := parseCompareArgs([]string{"--preset", "11", "--target-vmaf", "90", "--vmaf-floor", "89", "--max-encoded-percent", "1000", "--no-progress", path})
	if err != nil {
		t.Fatal(err)
	}
	info, err := probeInputMediaContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		result, err := compareInput(context.Background(), opts, info)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Results) != 2 {
			t.Fatal(result)
		}
		for _, row := range result.Results {
			if !row.Success || row.CacheHit != (pass == 1) {
				t.Fatalf("bad comparison pass %d: %+v", pass, row)
			}
			if pass == 0 && (row.Timing.EncodeSeconds <= 0 || row.Timing.ScoreSeconds <= 0) {
				t.Fatal("missing fresh timing")
			}
			if pass == 1 && (row.Timing.EncodeSeconds != 0 || row.Timing.ScoreSeconds != 0 || row.Timing.InspectionSeconds != 0 || row.Timing.TotalSeconds <= 0 || row.Timing.CacheSeconds <= 0) {
				t.Fatal("incorrect current-invocation cached timing", row.Timing)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source changed", err)
	}
	if _, err := os.Stat(outputPathFor(path)); !os.IsNotExist(err) {
		t.Fatal("final output created")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatal("scratch leak", entry.Name())
		}
	}
}
