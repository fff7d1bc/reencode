package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func orderedTestSearch() *crfSearch {
	s := &crfSearch{info: MediaInfo{Path: "input", Duration: 10 * time.Second, VideoBytes: 100000}, options: defaultProbeOptions(), attempts: make(map[int]ProbeAttempt)}
	s.options.NoCache = true
	for i := 0; i < 4; i++ {
		s.samples = append(s.samples, SampleFile{Start: time.Duration(i+1) * time.Second, Duration: time.Second})
	}
	return s
}

func TestWindowOrderNearestCRFAndStableTies(t *testing.T) {
	s := orderedTestSearch()
	if got := s.windowOrder(128); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Fatal(got)
	}
	s.rememberWindowScore(s.samples[0], 120, 98)
	s.rememberWindowScore(s.samples[0], 136, 88) // Equal distance must prefer lower CRF, not lower score.
	s.rememberWindowScore(s.samples[1], 127, 94)
	s.rememberWindowScore(s.samples[2], 129, 94)
	for range 20 {
		if got := s.windowOrder(128); !reflect.DeepEqual(got, []int{1, 2, 0, 3}) {
			t.Fatal(got)
		}
	}
	if got := s.windowOrder(135); !reflect.DeepEqual(got, []int{0, 1, 2, 3}) {
		t.Fatal(got)
	}
}

func TestWindowOrderFailFastAndCanonicalReports(t *testing.T) {
	old := measureProbeWindow
	defer func() { measureProbeWindow = old }()
	s := orderedTestSearch()
	var calls []int
	measureProbeWindow = func(_ *crfSearch, _ context.Context, video VideoArgs, sample SampleFile, n int, _ probeProgress) (windowMeasurement, error) {
		calls = append(calls, n)
		score := 98.0
		if n == 3 {
			score = 94.5
			if video.CRF > 32 {
				score = 90
			}
		}
		return testMeasurement(sample, score), nil
	}
	first, err := s.evaluate(context.Background(), 128)
	if err != nil || !reflect.DeepEqual(calls, []int{1, 2, 3, 4}) || first.partial {
		t.Fatal(calls, first, err)
	}
	calls = nil
	failed, err := s.evaluate(context.Background(), 132)
	if err != nil || !reflect.DeepEqual(calls, []int{3}) || !failed.partial || failed.samplesDone != 1 {
		t.Fatal(calls, failed, err)
	}
	calls = nil
	passed, err := s.evaluate(context.Background(), 124)
	if err != nil || !reflect.DeepEqual(calls, []int{3, 1, 2, 4}) || passed.partial {
		t.Fatal(calls, passed, err)
	}
	for i, d := range passed.Samples {
		if d.StartSeconds != s.samples[i].Start.Seconds() {
			t.Fatal("report reordered", passed)
		}
	}
	if len(passed.sampleScores) != 4 || passed.sampleScores[2] != 94.5 {
		t.Fatal("outlier index changed", passed.sampleScores)
	}
}

func TestPartialSizeCannotStopCRFSearch(t *testing.T) {
	s := orderedTestSearch()
	// Seed every CRF so the search test is entirely deterministic. The first
	// difficult-window rejection has a misleading size estimate over the cap.
	for q := qFromCRF(defaultCRFMin); q <= qFromCRF(defaultCRFMax); q++ {
		a := ProbeAttempt{CRF: crfFromQ(q), Score: 96, WorstSampleScore: 95, EncodedPercent: 80}
		if q > 128 {
			a.Score = 90
			a.WorstSampleScore = 90
			a.EncodedPercent = 200
			a.partial = true
		}
		s.attempts[q] = a
	}
	best, ok, err := s.findForTarget(context.Background(), 95)
	if err != nil || !ok || best.CRF != 32 {
		t.Fatal(best, ok, err)
	}
	if groupQualityOK(ProbeAttempt{Score: 100, WorstSampleScore: 100, partial: true}, s.options) {
		t.Fatal("group accepted incomplete coverage")
	}
}
