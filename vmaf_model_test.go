package main

import (
	"context"
	"crypto/sha512"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVMAFProfileSelection(t *testing.T) {
	for _, tt := range []struct {
		w, h          int
		fps           float64
		suffix, scale string
	}{
		{1920, 1080, 24, "_3d0h", ""}, {1080, 1920, 60, "_hfr_3d0h", ""},
		{3840, 2160, 30, "_1d5h_2160", ""}, {2160, 3840, 45, "_hfr_1d5h_2160", ""},
		{720, 1280, 44.9, "_3d0h", "scale=-1:1920:flags=bicubic,"},
		{1280, 720, 30, "_3d0h", "scale=1920:-1:flags=bicubic,"},
	} {
		m := resolveVMAFModel(defaultProbeOptions(), MediaInfo{Width: tt.w, Height: tt.h, FPS: tt.fps})
		if m.ID != "vmaf_v1.0.16"+tt.suffix || m.Scale != tt.scale {
			t.Fatalf("unexpected model: %+v", m)
		}
		data, err := vmafAssets.ReadFile("models/" + m.ID + ".json")
		if err != nil || m.SHA512 != fmt.Sprintf("%x", sha512.Sum512(data)) {
			t.Fatal("bad embedded identity", err)
		}
		filter := m.filter("/tmp/model.json")
		for _, part := range []string{fmt.Sprintf("cambi.enc_width=%d", tt.w), fmt.Sprintf("cambi.enc_height=%d", tt.h), "cambi.enc_bitdepth=10", "pool=harmonic_mean", "format=yuv420p10le"} {
			if !strings.Contains(filter, part) {
				t.Fatal(filter)
			}
		}
	}
	opts := defaultProbeOptions()
	opts.VMAFProfile = "legacy"
	info := MediaInfo{Width: 1080, Height: 1920, FPS: 60}
	if m := resolveVMAFModel(opts, info); m.filter("") != vmafFilter(info) || m.SHA512 != "" {
		t.Fatal(m)
	}
	if _, _, err := parseProbeArgs([]string{"--vmaf-profile", "unknown", "file"}); err == nil {
		t.Fatal("accepted invalid profile")
	}
}

func TestVMAFRuntimeVersions(t *testing.T) {
	for version, want := range map[string]bool{"3.2.0": false, "3.2.1": true, "v3.2.1-0-gabc": true, "3.3.0": true, "4.0.0": true, "garbage": false, "f85a853": true} {
		if got := versionAtLeast(version, 3, 2, 1); got != want {
			t.Fatalf("%s: %v", version, got)
		}
	}
}

func TestModelCacheIdentityAndCleanup(t *testing.T) {
	opts := defaultProbeOptions()
	info := MediaInfo{Width: 1920, Height: 1080, FPS: 30}
	base := normalizedProbeCacheOptionsForMedia(opts, info)
	for _, change := range []func(*ProbeOptions, *MediaInfo){
		func(o *ProbeOptions, _ *MediaInfo) { o.VMAFProfile = "legacy" },
		func(_ *ProbeOptions, i *MediaInfo) { i.FPS = 60 },
		func(o *ProbeOptions, _ *MediaInfo) { o.runtime.LibVMAF = "3.2.1" },
		func(o *ProbeOptions, _ *MediaInfo) { o.runtime.SVTAV1 = "v4.2.0" },
	} {
		o, i := opts, info
		change(&o, &i)
		if normalizedProbeCacheOptionsForMedia(o, i) == base {
			t.Fatal("cache identity unchanged")
		}
	}
	opts.KeepTemp = true
	path, err := materializeVMAFModel(base.Model, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := crfSearch{options: opts, modelPath: path}
	s.cleanupEncodedSamples()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("model retained", err)
	}
	if _, err := materializeVMAFModel(base.Model, filepath.Join(t.TempDir(), "pipe|path")); err == nil {
		t.Fatal("pipe accepted")
	}
}

func TestMediaIntegrationV1Models(t *testing.T) {
	requireMediaIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts := defaultProbeOptions()
	opts.TempDir = filepath.Join(t.TempDir(), "models' [test], colon: space")
	for _, info := range []MediaInfo{{Width: 1920, Height: 1080, FPS: 30}, {Width: 1920, Height: 1080, FPS: 60}, {Width: 3840, Height: 2160, FPS: 30}, {Width: 2160, Height: 3840, FPS: 60}} {
		m := resolveVMAFModel(opts, info)
		versions, err := detectScoringRuntime(ctx, opts, m)
		if err != nil {
			t.Fatal(err)
		}
		if versions.LibVMAF == "" || versions.SVTAV1 == "" {
			t.Fatal(versions)
		}
	}
	entries, err := os.ReadDir(opts.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("runtime scratch leak", entries, err)
	}
}
