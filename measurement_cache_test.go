package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testMeasurement(sample SampleFile, score float64) windowMeasurement {
	timeline := frameTimeline{Start: sample.Start, Times: []time.Duration{0, sample.Duration / 2}, Duration: sample.Duration}
	return windowMeasurement{Timeline: timeline, EncodedBytes: 2048, Diagnostic: SampleDiagnostics{
		StartSeconds: sample.Start.Seconds(), DurationSeconds: sample.Duration.Seconds(), Frames: 2,
		Score: score, FrameP05: score, WorstSecondScore: score, WorstSecondStart: sample.Start.Seconds(), WorstSecondDuration: min(time.Second, sample.Duration).Seconds(),
	}}
}

func TestMeasurementIdentitySeparatesMeasurementFromDecision(t *testing.T) {
	opts := defaultProbeOptions()
	opts.captureBackend = capturePolicy
	info := MediaInfo{Path: "first", Duration: time.Hour, FPS: 24, Width: 1920, Height: 1080, VideoIndex: 1}
	sample := SampleFile{Start: time.Second, Duration: time.Second}
	video := buildVideoArgs(info, opts.Preset, 32.25, opts.Tune)
	key, err := measurementKey(opts, info, sample, video)
	if err != nil {
		t.Fatal(err)
	}
	changed := opts
	changed.TargetVMAF, changed.FloorVMAF, changed.MaxEncodedPercent = 90, 89, 50
	changed.NoOutlierCheck, changed.JSON, changed.NoProgress, changed.RefreshCache = true, true, true, true
	changed.Samples, changed.SampleDuration, changed.TempDir = 2, time.Minute, "elsewhere"
	rename := info
	rename.Path = "renamed"
	if other, _ := measurementKey(changed, rename, sample, video); other != key {
		t.Fatal("decision/UI/rename split measurement")
	}
	for _, change := range []func(*ProbeOptions, *MediaInfo, *SampleFile, *VideoArgs){
		func(o *ProbeOptions, _ *MediaInfo, _ *SampleFile, _ *VideoArgs) { o.captureBackend = inspectionPolicy },
		func(o *ProbeOptions, _ *MediaInfo, _ *SampleFile, _ *VideoArgs) { o.VMAFProfile = "legacy" },
		func(o *ProbeOptions, _ *MediaInfo, _ *SampleFile, _ *VideoArgs) { o.runtime.LibVMAF = "new" },
		func(_ *ProbeOptions, i *MediaInfo, _ *SampleFile, _ *VideoArgs) { i.StartTime++ },
		func(_ *ProbeOptions, i *MediaInfo, _ *SampleFile, _ *VideoArgs) { i.VideoIndex++ },
		func(_ *ProbeOptions, _ *MediaInfo, s *SampleFile, _ *VideoArgs) { s.Start++ },
		func(_ *ProbeOptions, _ *MediaInfo, s *SampleFile, _ *VideoArgs) { s.Duration++ },
		func(_ *ProbeOptions, _ *MediaInfo, s *SampleFile, _ *VideoArgs) { s.FullPass = true },
		func(_ *ProbeOptions, _ *MediaInfo, _ *SampleFile, v *VideoArgs) { v.CRF += .25 },
		func(_ *ProbeOptions, _ *MediaInfo, _ *SampleFile, v *VideoArgs) { v.Keyint++ },
		func(_ *ProbeOptions, _ *MediaInfo, _ *SampleFile, v *VideoArgs) { v.Tune = "vq" },
	} {
		o, i, s, v := opts, info, sample, video
		change(&o, &i, &s, &v)
		if other, _ := measurementKey(o, i, s, v); other == key {
			t.Fatal("measurement change reused identity")
		}
	}
	base := probeCacheHandle{Root: "root", BinaryHash: "binary", ExternalToolsHash: "tools", FingerprintKey: "input"}
	for _, change := range []func(*probeCacheHandle){func(c *probeCacheHandle) { c.BinaryHash = "other" }, func(c *probeCacheHandle) { c.ExternalToolsHash = "other" }, func(c *probeCacheHandle) { c.FingerprintKey = "other" }} {
		other := base
		change(&other)
		if measurementPath(&base, key) == measurementPath(&other, key) {
			t.Fatal("cache scope collision")
		}
	}
}

func TestMeasurementResumeAfterInterruption(t *testing.T) {
	old := measureProbeWindow
	defer func() { measureProbeWindow = old }()
	cache := &probeCacheHandle{Root: t.TempDir(), BinaryHash: "binary", ExternalToolsHash: "tools", FingerprintKey: "input"}
	opts := defaultProbeOptions()
	info := MediaInfo{Path: "input", Duration: 10 * time.Second, VideoBytes: 100000}
	samples := []SampleFile{{SourcePath: "input", Start: time.Second, Duration: time.Second}, {SourcePath: "input", Start: 3 * time.Second, Duration: time.Second}}
	newSearch := func() *crfSearch {
		return &crfSearch{info: info, options: opts, samples: samples, attempts: make(map[int]ProbeAttempt), cache: cache}
	}
	measureProbeWindow = func(_ *crfSearch, _ context.Context, _ VideoArgs, sample SampleFile, n int, _ probeProgress) (windowMeasurement, error) {
		if n == 2 {
			return windowMeasurement{}, context.Canceled
		}
		return testMeasurement(sample, 93.5), nil
	}
	if _, err := newSearch().evaluate(context.Background(), 128); err != context.Canceled {
		t.Fatal(err)
	}
	opts.TargetVMAF, opts.FloorVMAF = 93, 92
	calls := 0
	measureProbeWindow = func(_ *crfSearch, _ context.Context, _ VideoArgs, sample SampleFile, n int, _ probeProgress) (windowMeasurement, error) {
		calls++
		if n != 2 {
			t.Fatal("completed first window repeated")
		}
		return testMeasurement(sample, 95), nil
	}
	search := newSearch()
	attempt, err := search.evaluate(context.Background(), 128)
	if err != nil || calls != 1 || search.reusedMeasurements != 1 || len(attempt.Samples) != 2 || !search.qualityOK(attempt, opts.TargetVMAF) {
		t.Fatal(attempt, calls, search.reusedMeasurements, err)
	}
}

func TestMeasurementCachePoliciesAndCorruption(t *testing.T) {
	old := measureProbeWindow
	defer func() { measureProbeWindow = old }()
	opts := defaultProbeOptions()
	info := MediaInfo{Path: "input", Duration: 10 * time.Second, VideoBytes: 100000}
	sample := SampleFile{Start: time.Second, Duration: time.Second}
	video := buildVideoArgs(info, opts.Preset, 32)
	cache := &probeCacheHandle{Root: t.TempDir(), BinaryHash: "binary", ExternalToolsHash: "tools", FingerprintKey: "input"}
	key, _ := measurementKey(opts, info, sample, video)
	path := measurementPath(cache, key)
	fresh := func(o ProbeOptions) *crfSearch { return &crfSearch{info: info, options: o, cache: cache} }
	calls := 0
	measureProbeWindow = func(_ *crfSearch, _ context.Context, _ VideoArgs, s SampleFile, _ int, _ probeProgress) (windowMeasurement, error) {
		calls++
		return testMeasurement(s, 95), nil
	}
	measure := func(s *crfSearch) {
		t.Helper()
		if _, _, _, err := s.measureWindow(context.Background(), video, sample, 1, probeProgress{}); err != nil {
			t.Fatal(err)
		}
	}
	measure(fresh(opts))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	noCache := opts
	noCache.NoCache = true
	measure(fresh(noCache))
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || calls != 2 {
		t.Fatal("no-cache read/wrote disk", calls)
	}
	refresh := opts
	refresh.RefreshCache = true
	search := fresh(refresh)
	measure(search)
	measure(search)
	if calls != 3 || search.reusedMeasurements != 1 {
		t.Fatal("refresh didn't bypass disk or reuse in-run work", calls)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	measure(fresh(opts))
	if calls != 4 {
		t.Fatal("corrupt cache reused")
	}
	m, ok, err := loadMeasurement(path, key, sample, qFromCRF(video.CRF))
	if err != nil || !ok {
		t.Fatal(err)
	}
	m.Diagnostic.Frames++
	if err := writeCacheJSON(path, m); err != nil {
		t.Fatal(err)
	}
	measure(fresh(opts))
	if calls != 5 {
		t.Fatal("incomplete cache reused")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatal("atomic-write scratch leak", e.Name())
		}
	}
}

func TestMeasurementRejectsChangedSourceBeforePersistence(t *testing.T) {
	old := measureProbeWindow
	defer func() { measureProbeWindow = old }()
	path := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	stat, _ := os.Stat(path)
	search := crfSearch{info: MediaInfo{Path: path, Duration: time.Second, VideoBytes: 10000, facts: &sourceFacts{source: snapshotSource(stat), timelines: make(map[sampleWindowKey]frameTimeline)}}, options: defaultProbeOptions(), cache: &probeCacheHandle{Root: t.TempDir()}}
	measureProbeWindow = func(_ *crfSearch, _ context.Context, _ VideoArgs, s SampleFile, _ int, _ probeProgress) (windowMeasurement, error) {
		if err := os.WriteFile(path, []byte("changed source"), 0600); err != nil {
			t.Fatal(err)
		}
		return testMeasurement(s, 95), nil
	}
	if _, _, _, err := search.measureWindow(context.Background(), buildVideoArgs(search.info, "11", 32), SampleFile{FullPass: true, Duration: time.Second}, 1, probeProgress{}); err == nil {
		t.Fatal("changed source persisted")
	}
	if len(search.measurements) != 0 {
		t.Fatal("changed source retained in memory")
	}
}

func TestMediaIntegrationResumeWindowMeasurements(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.mov")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=12", "-t", "1", "-c:v", "mpeg4", "-q:v", "2", path)
	opts := defaultProbeOptions()
	opts.Preset, opts.NoProgress, opts.TargetVMAF, opts.FloorVMAF, opts.MaxEncodedPercent = "11", true, 90, 89, 1000
	result, session, cache, err := probeFile(context.Background(), opts, path, false)
	if session != nil {
		session.Close()
	}
	if err != nil || !result.Success || cache == nil {
		t.Fatal(result, err)
	}
	if err := os.Remove(cache.Path); err != nil {
		t.Fatal(err)
	}
	resumed, err := ProbeFile(context.Background(), opts, path)
	if err != nil || !resumed.Success || resumed.CacheHit || resumed.ReusedMeasurements == 0 || resumed.CRF != result.CRF || resumed.Timing.EncodeSeconds != 0 || resumed.Timing.ScoreSeconds != 0 {
		t.Fatal("window resume repeated media work", resumed, err)
	}
}
