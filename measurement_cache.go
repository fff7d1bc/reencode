package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

const measurementSchema = 1

type windowMeasurement struct {
	Schema       int               `json:"schema"`
	Key          string            `json:"key"`
	Window       sampleWindowKey   `json:"window"`
	Q            int               `json:"crf_quarter"`
	Timeline     frameTimeline     `json:"timeline"`
	Diagnostic   SampleDiagnostics `json:"diagnostic"`
	EncodedBytes int64             `json:"encoded_bytes"`
}

func measurementKey(opts ProbeOptions, info MediaInfo, sample SampleFile, video VideoArgs) (string, error) {
	// Decision thresholds and sample-plan scheduling do not change an exact
	// window measurement. Actual encoder arguments and scoring policy do.
	info.Path = ""
	identity := struct {
		Schema       int
		Media        MediaInfo
		Video        []string
		Model        VMAFModelInfo
		Runtime      RuntimeVersions
		TimingPolicy string
		Window       sampleWindowKey
		Q            int
	}{measurementSchema, info, video.ffmpegArgs("-c:v"), resolveVMAFModel(opts, info), opts.runtime, opts.captureBackend,
		sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}, qFromCRF(video.CRF)}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:]), nil
}

func measurementPath(cache *probeCacheHandle, key string) string {
	if cache == nil {
		return ""
	}
	return filepath.Join(cache.Root, cache.BinaryHash, cache.ExternalToolsHash, cache.FingerprintKey, "windows-v1", key+".json")
}

func validateMeasurement(m windowMeasurement, key string, sample SampleFile, q int) error {
	window := sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}
	d, timeline := m.Diagnostic, m.Timeline
	for _, value := range []float64{d.StartSeconds, d.DurationSeconds, d.WorstSecondStart, d.WorstSecondDuration} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("non-finite window diagnostics")
		}
	}
	if m.Schema != measurementSchema || m.Key != key || m.Window != window || m.Q != q || m.EncodedBytes <= 1024 ||
		len(timeline.Times) == 0 || timeline.Times[0] != 0 || timeline.Duration <= timeline.Times[len(timeline.Times)-1] ||
		d.Frames != len(timeline.Times) || math.Abs(d.StartSeconds-timeline.Start.Seconds()) > 1e-9 || math.Abs(d.DurationSeconds-timeline.Duration.Seconds()) > 1e-9 ||
		!validVMAFScore(d.Score) || !validVMAFScore(d.FrameP05) || !validVMAFScore(d.WorstSecondScore) ||
		d.WorstSecondDuration <= 0 ||
		d.WorstSecondStart < d.StartSeconds-1e-9 || d.WorstSecondStart+d.WorstSecondDuration > d.StartSeconds+d.DurationSeconds+1e-9 {
		return fmt.Errorf("invalid window measurement")
	}
	for i := 1; i < len(timeline.Times); i++ {
		if timeline.Times[i] <= timeline.Times[i-1] {
			return fmt.Errorf("invalid cached frame timestamps")
		}
	}
	if !sample.FullPass && (timeline.Start < sample.Start || timeline.Start+timeline.Duration > sample.Start+sample.Duration) {
		return fmt.Errorf("cached timeline outside sample window")
	}
	return nil
}

func loadMeasurement(path, key string, sample SampleFile, q int) (windowMeasurement, bool, error) {
	var m windowMeasurement
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, false, err
	}
	if err = validateMeasurement(m, key, sample, q); err != nil {
		return m, false, err
	}
	return m, true, nil
}

func (s *crfSearch) initMeasurements() {
	if s.measurements != nil {
		return
	}
	if s.info.facts != nil {
		if s.info.facts.measurements == nil {
			s.info.facts.measurements = make(map[string]windowMeasurement)
		}
		s.measurements = s.info.facts.measurements
	} else {
		s.measurements = make(map[string]windowMeasurement)
	}
}

var measureProbeWindow = (*crfSearch).measureFreshWindow

func (s *crfSearch) measureWindow(ctx context.Context, video VideoArgs, sample SampleFile, number int, progress probeProgress) (SampleFile, SampleDiagnostics, int64, error) {
	if err := ctx.Err(); err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	if err := s.info.facts.check(s.info.Path); err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	started := time.Now()
	key, err := measurementKey(s.options, s.info, sample, video)
	if err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	s.initMeasurements()
	m, ok := s.measurements[key]
	path := ""
	if !s.options.NoCache {
		path = measurementPath(s.cache, key)
	}
	if !ok && path != "" && !s.options.RefreshCache {
		m, ok, err = loadMeasurement(path, key, sample, qFromCRF(video.CRF))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: window cache read failed, measuring again: %v\n", displayPath(s.info.Path), err)
		}
	}
	s.timing.CacheSeconds += time.Since(started).Seconds()
	if ok {
		if err := s.info.facts.check(s.info.Path); err != nil {
			return sample, SampleDiagnostics{}, 0, err
		}
		s.initTimelines()
		s.timelines[m.Window] = m.Timeline
		s.measurements[key] = m
		s.reusedMeasurements++
		prepared, err := s.applyTimeline(sample, m.Timeline)
		return prepared, m.Diagnostic, m.EncodedBytes, err
	}
	m, err = measureProbeWindow(s, ctx, video, sample, number, progress)
	if err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	if err := s.info.facts.check(s.info.Path); err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	m.Schema, m.Key, m.Q, m.Window = measurementSchema, key, qFromCRF(video.CRF), sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}
	if err := validateMeasurement(m, key, sample, m.Q); err != nil {
		return sample, SampleDiagnostics{}, 0, err
	}
	// Only complete, aligned windows reach this point. Persist before deciding
	// whether their scores pass: a quality miss is still a reusable measurement.
	s.measurements[key] = m
	s.initTimelines()
	s.timelines[m.Window] = m.Timeline
	if path != "" {
		started = time.Now()
		if err := writeCacheJSON(path, m); err != nil {
			fmt.Fprintf(os.Stderr, "%s: window cache write failed: %v\n", displayPath(s.info.Path), err)
		}
		s.timing.CacheSeconds += time.Since(started).Seconds()
	}
	prepared, err := s.applyTimeline(sample, m.Timeline)
	return prepared, m.Diagnostic, m.EncodedBytes, err
}

func (s *crfSearch) measureFreshWindow(ctx context.Context, video VideoArgs, sample SampleFile, number int, progress probeProgress) (windowMeasurement, error) {
	var err error
	sample, err = s.prepareProbeSample(ctx, sample)
	if err != nil {
		return windowMeasurement{}, err
	}
	encoded, size, err := s.encodeSample(ctx, video, sample, number, progress)
	if err != nil {
		return windowMeasurement{}, err
	}
	if !s.options.KeepTemp {
		defer os.Remove(encoded)
	}
	progress.Done++
	diagnostic, err := s.scoreSample(ctx, sample, encoded, progress)
	if err != nil {
		return windowMeasurement{}, err
	}
	s.initTimelines()
	window := sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}
	timeline := s.timelines[window]
	return windowMeasurement{Timeline: timeline, Diagnostic: diagnostic, EncodedBytes: size}, nil
}
