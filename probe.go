package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"
)

type ProbeOptions struct {
	Preset            string
	JSON              bool
	TargetVMAF        float64
	FloorVMAF         float64
	MaxEncodedPercent float64
	NoOutlierCheck    bool
	NoCache           bool
	RefreshCache      bool
	SkipNames         []string
	CheckWorkers      int
	Samples           int
	SampleDuration    time.Duration
	TempDir           string
	KeepTemp          bool
	StallTimeout      time.Duration
	NoProgress        bool
	Verbose           bool
	Progress          *ProgressDisplay
}

type ProbeAttempt struct {
	CRF              float64 `json:"crf"`
	Score            float64 `json:"score"`
	WorstSampleScore float64 `json:"worst_sample_score"`
	EncodedPercent   float64 `json:"encoded_percent"`
	PredictedSize    int64   `json:"predicted_size"`
	EffectiveTarget  float64 `json:"effective_target,omitempty"`
	OutlierChecked   bool    `json:"outlier_checked,omitempty"`
	OutlierAccepted  bool    `json:"outlier_accepted,omitempty"`
	OutlierScore     float64 `json:"outlier_score,omitempty"`

	OutlierNeighborScores []float64 `json:"outlier_neighbor_scores,omitempty"`
	// sampleScores is intentionally not exported to JSON. It is only needed
	// while the process is alive to identify a single borderline low sample.
	sampleScores []float64
}

type ProbeResult struct {
	File             string         `json:"file"`
	Success          bool           `json:"success"`
	CRF              float64        `json:"crf,omitempty"`
	TargetVMAF       float64        `json:"target_vmaf"`
	FloorVMAF        float64        `json:"floor_vmaf"`
	EffectiveTarget  float64        `json:"effective_target,omitempty"`
	Score            float64        `json:"score,omitempty"`
	WorstSampleScore float64        `json:"worst_sample_score,omitempty"`
	EncodedPercent   float64        `json:"encoded_percent,omitempty"`
	PredictedSize    int64          `json:"predicted_size,omitempty"`
	OutlierChecked   bool           `json:"outlier_checked,omitempty"`
	OutlierAccepted  bool           `json:"outlier_accepted,omitempty"`
	OutlierScore     float64        `json:"outlier_score,omitempty"`
	SamplePlan       SamplePlan     `json:"sample_plan"`
	Attempts         []ProbeAttempt `json:"attempts"`
	Error            string         `json:"error,omitempty"`

	OutlierNeighborScores []float64 `json:"outlier_neighbor_scores,omitempty"`
}

func runProbeCommand(ctx context.Context, opts ProbeOptions, files []string) int {
	if ctx.Err() != nil {
		return 130
	}
	if err := checkFFmpegPreflight(ctx, ffmpegRequirements{SVTAV1: true, VMAF: true}); err != nil {
		if ctx.Err() != nil {
			return 130
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	printEligibilityStart(files, opts.JSON)
	eligibility := collectEligibleInputs(ctx, eligibilityProbe, files, EncodeOptions{ProbeOptions: opts})
	if eligibility.Fatal != nil {
		fmt.Fprintf(os.Stderr, "%v\n", eligibility.Fatal)
		return 1
	}
	printEligibilitySummary(eligibility, len(files), opts.JSON)
	exitCode := 0
	enc := json.NewEncoder(os.Stdout)
	if !opts.JSON && !shouldSummarizeEligibility(files, opts.JSON) {
		for _, skipped := range eligibility.Skipped {
			printSkippedInput(skipped)
		}
	}
	for _, entry := range eligibility.Entries {
		if ctx.Err() != nil {
			return 130
		}
		if entry.Skipped {
			if opts.JSON {
				_ = enc.Encode(probeSkippedResult(skippedInput{File: entry.File, Reason: entry.SkipReason, Pattern: entry.Pattern}, opts))
			}
			continue
		}
		file := entry.File
		result, err := ProbeFile(ctx, opts, file)
		if err != nil {
			if ctx.Err() != nil {
				return 130
			}
			if errors.Is(err, errNotVideoFile) {
				if opts.JSON {
					_ = enc.Encode(ProbeResult{File: file, TargetVMAF: opts.TargetVMAF, FloorVMAF: opts.FloorVMAF, Error: "not a video file, skipped"})
				} else {
					fmt.Fprintf(os.Stderr, "%s: not a video file, skipping\n", displayPath(file))
				}
				continue
			}
			if errors.Is(err, errNoVideoStream) {
				if opts.JSON {
					_ = enc.Encode(ProbeResult{File: file, TargetVMAF: opts.TargetVMAF, FloorVMAF: opts.FloorVMAF, Error: "no video stream found, skipped"})
				} else {
					fmt.Fprintf(os.Stderr, "%s: no video stream found, skipping\n", displayPath(file))
				}
				continue
			}
			result.File = file
			result.TargetVMAF = opts.TargetVMAF
			result.FloorVMAF = opts.FloorVMAF
			result.Error = err.Error()
			exitCode = 1
		}
		if opts.JSON {
			_ = enc.Encode(result)
			continue
		}
		printProbeHuman(result)
	}
	return exitCode
}

func printProbeHuman(result ProbeResult) {
	if result.Success {
		fmt.Printf("%s: encode would use crf %5s  VMAF %6.2f  worst %6.2f  size %4.0f%%  predicted %s\n",
			displayPath(result.File),
			terseFloat(result.CRF),
			result.Score,
			result.WorstSampleScore,
			result.EncodedPercent,
			humanBytes(result.PredictedSize),
		)
		if result.OutlierAccepted {
			fmt.Println(formatOutlierAcceptedLine(probeAttemptFromResult(result)))
		}
		return
	}
	fmt.Fprintf(os.Stderr, "%s: probe failed: %s\n", displayPath(result.File), result.Error)
}

func ProbeFile(ctx context.Context, opts ProbeOptions, file string) (ProbeResult, error) {
	result, session, _, err := probeFile(ctx, opts, file, false)
	if session != nil {
		session.Close()
	}
	return result, err
}

func probeFileSession(ctx context.Context, opts ProbeOptions, file string) (ProbeResult, *probeSession, *probeCacheHandle, error) {
	return probeFile(ctx, opts, file, true)
}

func probeFile(ctx context.Context, opts ProbeOptions, file string, keepSession bool) (ProbeResult, *probeSession, *probeCacheHandle, error) {
	info, err := probeInputMedia(file)
	if err != nil {
		var result ProbeResult
		result.File = file
		result.TargetVMAF = opts.TargetVMAF
		result.FloorVMAF = opts.FloorVMAF
		return result, nil, nil, err
	}
	cache, err := prepareProbeCache(opts, file)
	if err != nil {
		warnCache(opts, "probe cache disabled: %v", err)
	} else if cache != nil && !opts.RefreshCache {
		result, ok, err := loadProbeCache(cache, file)
		if err != nil {
			warnCache(opts, "probe cache read failed: %v", err)
		} else if ok {
			warnCache(opts, "probe cache hit: %s", displayPath(file))
			if !keepSession {
				return result, nil, cache, nil
			}
			session, err := newProbeSession(opts, info)
			if err != nil {
				return result, nil, cache, err
			}
			seedProbeSessionAttempts(session, result)
			return result, session, cache, nil
		}
	}

	session, err := newProbeSession(opts, info)
	if err != nil {
		var result ProbeResult
		result.File = file
		result.TargetVMAF = opts.TargetVMAF
		result.FloorVMAF = opts.FloorVMAF
		return result, nil, cache, err
	}

	result, err := session.Run(ctx)
	if err != nil {
		return result, session, cache, err
	}
	if cache != nil && result.Success {
		if err := storeProbeCache(cache, result); err != nil {
			warnCache(opts, "probe cache write failed: %v", err)
		}
	}
	if !keepSession {
		session.Close()
		return result, nil, cache, nil
	}
	return result, session, cache, nil
}

type probeSession struct {
	result ProbeResult
	search crfSearch
}

func newProbeSession(opts ProbeOptions, info MediaInfo) (*probeSession, error) {
	plan := planSamples(info, opts.Samples, opts.SampleDuration)
	result := ProbeResult{
		File:       info.Path,
		TargetVMAF: opts.TargetVMAF,
		FloorVMAF:  opts.FloorVMAF,
		SamplePlan: plan,
	}
	samples, err := createSamples(info, plan)
	if err != nil {
		return &probeSession{result: result}, err
	}

	search := crfSearch{
		info:                       info,
		samples:                    samples,
		options:                    opts,
		encodedSamplePaths:         []string{},
		attempts:                   map[int]ProbeAttempt{},
		reportedAttempts:           map[int]bool{},
		reportedOutlierAcceptances: map[int]bool{},
	}
	return &probeSession{result: result, search: search}, nil
}

func seedProbeSessionAttempts(session *probeSession, result ProbeResult) {
	session.result = result
	if session.search.attempts == nil {
		session.search.attempts = map[int]ProbeAttempt{}
	}
	// Cached probe results include every completed attempt. Seeding them here
	// lets group mode test a shared CRF without re-encoding samples it already
	// measured in a previous probe.
	for _, attempt := range result.Attempts {
		session.search.attempts[qFromCRF(attempt.CRF)] = attempt
	}
	if result.Success {
		selected := probeAttemptFromResult(result)
		session.search.attempts[qFromCRF(selected.CRF)] = selected
	}
}

func probeAttemptFromResult(result ProbeResult) ProbeAttempt {
	return ProbeAttempt{
		CRF:                   result.CRF,
		Score:                 result.Score,
		WorstSampleScore:      result.WorstSampleScore,
		EncodedPercent:        result.EncodedPercent,
		PredictedSize:         result.PredictedSize,
		EffectiveTarget:       result.EffectiveTarget,
		OutlierChecked:        result.OutlierChecked,
		OutlierAccepted:       result.OutlierAccepted,
		OutlierScore:          result.OutlierScore,
		OutlierNeighborScores: result.OutlierNeighborScores,
	}
}

func (s *probeSession) Run(ctx context.Context) (ProbeResult, error) {
	best, effectiveTarget, err := s.search.find(ctx)
	s.result.Attempts = s.search.sortedAttempts()
	if err != nil {
		return s.result, err
	}
	s.search.reportSelectedAttempt(best)
	s.result.Success = true
	s.result.CRF = best.CRF
	s.result.EffectiveTarget = effectiveTarget
	s.result.Score = best.Score
	s.result.WorstSampleScore = best.WorstSampleScore
	s.result.EncodedPercent = best.EncodedPercent
	s.result.PredictedSize = best.PredictedSize
	s.result.OutlierChecked = best.OutlierChecked
	s.result.OutlierAccepted = best.OutlierAccepted
	s.result.OutlierScore = best.OutlierScore
	s.result.OutlierNeighborScores = best.OutlierNeighborScores
	return s.result, nil
}

func (s *probeSession) EvaluateCRF(ctx context.Context, crf float64) (ProbeAttempt, error) {
	q := qFromCRF(crf)
	attempt, err := s.search.evaluate(ctx, q)
	if err != nil {
		return ProbeAttempt{}, err
	}
	if attempt.Score >= s.search.options.TargetVMAF {
		// Group mode calls EvaluateCRF outside the normal search loop. It still
		// needs the same outlier confirmation as regular probe selection.
		attempt, err = s.search.maybeConfirmOutlier(ctx, q, s.search.options.TargetVMAF, attempt)
		if err != nil {
			return ProbeAttempt{}, err
		}
		s.search.printOutlierAcceptedProgress(q, attempt)
		return attempt, nil
	}
	return attempt, nil
}

func (s *probeSession) Close() {
	s.search.cleanupEncodedSamples()
}

type crfSearch struct {
	info                       MediaInfo
	samples                    []SampleFile
	options                    ProbeOptions
	attempts                   map[int]ProbeAttempt
	reportedAttempts           map[int]bool
	reportedOutlierAcceptances map[int]bool
	encodedSamplePaths         []string
}

func (s *crfSearch) find(ctx context.Context) (ProbeAttempt, float64, error) {
	targets := vmafTargets(s.options.TargetVMAF, s.options.FloorVMAF)
	var lastErr error
	// Try the requested target first, then step down toward the floor. This is
	// what makes one command cover "aim for 95, but accept down to 94" without
	// an external retry script.
	for _, target := range targets {
		best, ok, err := s.findForTarget(ctx, target)
		if err != nil {
			return ProbeAttempt{}, target, err
		}
		if ok {
			best.EffectiveTarget = target
			s.attempts[qFromCRF(best.CRF)] = best
			return best, target, nil
		}
		lastErr = fmt.Errorf("no CRF satisfied VMAF %.2f and max encoded percent %.0f", target, s.options.MaxEncodedPercent)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no CRF satisfied VMAF floor %.2f", s.options.FloorVMAF)
	}
	return ProbeAttempt{}, s.options.FloorVMAF, lastErr
}

func (s *crfSearch) findForTarget(ctx context.Context, target float64) (ProbeAttempt, bool, error) {
	minQ := qFromCRF(defaultCRFMin)
	maxQ := qFromCRF(defaultCRFMax)
	low, high := minQ, maxQ
	var best ProbeAttempt
	found := false
	seen := map[int]bool{}

	for low <= high {
		q := (low + high) / 2
		// Binary search is still the fallback, but interpolation often avoids a
		// few expensive sample encodes once both a passing and failing CRF exist.
		q = s.interpolateQ(target, q, low, high)
		if seen[q] {
			q = (low + high) / 2
		}
		seen[q] = true
		attempt, err := s.evaluate(ctx, q)
		if err != nil {
			return ProbeAttempt{}, false, err
		}
		passSize := attempt.EncodedPercent <= s.options.MaxEncodedPercent
		if attempt.Score >= target && passSize {
			attempt, err = s.maybeConfirmOutlier(ctx, q, target, attempt)
			if err != nil {
				return ProbeAttempt{}, false, err
			}
		}
		passScore := s.qualityOK(attempt, target)
		if passScore && passSize {
			best = attempt
			found = true
			// Higher CRF is smaller and lower quality, so after a pass we search
			// upward for the highest still-acceptable CRF.
			low = q + 1
			continue
		}
		if !passScore {
			// A quality miss means this CRF is too weak, so search downward.
			high = q - 1
			continue
		}
		// Quality passed but the sample is already larger than the source cap.
		// Search upward for a smaller encode while keeping the pass as unusable.
		low = q + 1
	}

	if refined, ok, err := s.refineBounds(ctx, target, best, found); err != nil {
		return ProbeAttempt{}, false, err
	} else if ok {
		return refined, true, nil
	}
	return best, found, nil
}

func (s *crfSearch) interpolateQ(target float64, fallback, low, high int) int {
	var better *ProbeAttempt
	var worse *ProbeAttempt
	for q, attempt := range s.attempts {
		if q < low || q > high {
			continue
		}
		passSize := attempt.EncodedPercent <= s.options.MaxEncodedPercent
		passScore := s.qualityOK(attempt, target)
		if passScore && passSize {
			a := attempt
			if better == nil || q > qFromCRF(better.CRF) {
				better = &a
			}
		}
		if !passScore {
			a := attempt
			if worse == nil || q < qFromCRF(worse.CRF) {
				worse = &a
			}
		}
	}
	if better == nil || worse == nil || better.Score <= worse.Score {
		return fallback
	}
	bq := qFromCRF(better.CRF)
	wq := qFromCRF(worse.CRF)
	if bq >= wq {
		return fallback
	}
	factor := (target - worse.Score) / (better.Score - worse.Score)
	q := int(math.Round(float64(wq) - float64(wq-bq)*factor))
	if q <= bq {
		q = bq + 1
	}
	if q >= wq {
		q = wq - 1
	}
	if q < low {
		q = low
	}
	if q > high {
		q = high
	}
	return q
}

func (s *crfSearch) refineBounds(ctx context.Context, target float64, best ProbeAttempt, found bool) (ProbeAttempt, bool, error) {
	if !found {
		return ProbeAttempt{}, false, nil
	}
	bestQ := qFromCRF(best.CRF)
	nextQ := bestQ + 1
	if nextQ > qFromCRF(defaultCRFMax) {
		return best, true, nil
	}
	// Interpolation can jump over the adjacent quarter-CRF. Checking best+0.25
	// avoids leaving an untested smaller encode on the table.
	next, err := s.evaluate(ctx, nextQ)
	if err != nil {
		return ProbeAttempt{}, false, err
	}
	if next.Score >= target && next.EncodedPercent <= s.options.MaxEncodedPercent {
		next, err = s.maybeConfirmOutlier(ctx, nextQ, target, next)
		if err != nil {
			return ProbeAttempt{}, false, err
		}
	}
	if s.qualityOK(next, target) && next.EncodedPercent <= s.options.MaxEncodedPercent {
		return next, true, nil
	}
	return best, true, nil
}

func (s *crfSearch) qualityOK(attempt ProbeAttempt, target float64) bool {
	return attempt.Score >= target && (attempt.WorstSampleScore >= s.options.FloorVMAF || attempt.OutlierAccepted)
}

func (s *crfSearch) evaluate(ctx context.Context, q int) (ProbeAttempt, error) {
	if attempt, ok := s.attempts[q]; ok {
		s.reportAttempt(q, attempt)
		return attempt, nil
	}
	crf := crfFromQ(q)
	var scores []sampleScore
	var totalSampleBytes int64
	var totalEncodedBytes int64
	var totalSampleDuration time.Duration
	var sampleScores []float64
	video := buildVideoArgs(s.info, s.options.Preset, crf)
	totalProbeOps := len(s.samples) * 2
	completedProbeOps := 0

	for i, sample := range s.samples {
		encoded, encodedSize, err := s.encodeSample(ctx, video, sample, i+1, probeProgressScope(crf, completedProbeOps, totalProbeOps))
		if err != nil {
			return ProbeAttempt{}, err
		}
		completedProbeOps++
		score, err := s.scoreSample(ctx, sample, encoded, probeProgressScope(crf, completedProbeOps, totalProbeOps))
		if err != nil {
			// scoreSample runs after a successful sample encode. Clean that
			// sample here because encodeSample's failure cleanup no longer owns it.
			if !s.options.KeepTemp {
				_ = os.Remove(encoded)
			}
			return ProbeAttempt{}, err
		}
		completedProbeOps++
		sampleScores = append(sampleScores, score)
		if !s.options.KeepTemp {
			_ = os.Remove(encoded)
		}
		scores = append(scores, sampleScore{Score: score, Duration: sample.Duration})
		totalSampleBytes += sample.SourceBytes
		totalEncodedBytes += encodedSize
		totalSampleDuration += sample.Duration
	}
	summary := summarizeScores(scores)

	encodedPercent := 100 * float64(totalEncodedBytes) / float64(totalSampleBytes)
	predictedSize := int64(float64(totalEncodedBytes) * (s.info.Duration.Seconds() / totalSampleDuration.Seconds()))
	if len(s.samples) == 1 && s.samples[0].Duration == s.info.Duration {
		// Full-pass probes already encoded the whole video stream.
		predictedSize = totalEncodedBytes
	}
	attempt := ProbeAttempt{
		CRF:              crf,
		Score:            summary.Mean,
		WorstSampleScore: summary.Worst,
		EncodedPercent:   encodedPercent,
		PredictedSize:    predictedSize,
		sampleScores:     sampleScores,
	}
	s.attempts[q] = attempt
	s.reportAttempt(q, attempt)
	return attempt, nil
}

func (s *crfSearch) reportAttempt(q int, attempt ProbeAttempt) {
	if s.options.Progress == nil {
		return
	}
	if s.reportedAttempts == nil {
		s.reportedAttempts = map[int]bool{}
	}
	if s.reportedAttempts[q] {
		return
	}
	// Cached attempts, retries at a lower effective target, and final
	// selection can all reuse a CRF without re-encoding it. Report each CRF once
	// so the selected line is never the first visible result for that CRF.
	s.options.Progress.PrintLine(formatProbeAttemptLine(attempt))
	s.reportedAttempts[q] = true
}

func (s *crfSearch) reportSelectedAttempt(attempt ProbeAttempt) {
	if s.options.Progress == nil {
		return
	}
	q := qFromCRF(attempt.CRF)
	s.reportAttempt(q, attempt)
	s.options.Progress.PrintLine(formatSelectedProbeAttemptLine(attempt))
	if attempt.OutlierAccepted {
		s.printOutlierAcceptedProgress(q, attempt)
	}
}

func formatProbeAttemptLine(attempt ProbeAttempt) string {
	return formatProbeAttemptLineWithPrefix(">>>", attempt)
}

func formatSelectedProbeAttemptLine(attempt ProbeAttempt) string {
	return formatProbeAttemptLineWithPrefix(">>> selected", attempt)
}

func formatProbeAttemptLineWithPrefix(prefix string, attempt ProbeAttempt) string {
	line := fmt.Sprintf("%s crf %5s  VMAF %6.2f  worst %6.2f  size %4.0f%%  predicted %s",
		prefix,
		terseFloat(attempt.CRF),
		attempt.Score,
		attempt.WorstSampleScore,
		attempt.EncodedPercent,
		humanBytes(attempt.PredictedSize),
	)
	return line
}

func formatOutlierAcceptedLine(attempt ProbeAttempt) string {
	return fmt.Sprintf(">>> accepted one local low sample %.2f: nearby windows passed the VMAF floor", attempt.OutlierScore)
}

type probeProgress struct {
	Label string
	Done  int
	Total int
}

const outlierFloorTolerance = 0.75

func probeProgressScope(crf float64, done, total int) probeProgress {
	return probeProgress{
		Label: "probe crf " + terseFloat(crf),
		Done:  done,
		Total: total,
	}
}

func outlierProgressScope(crf float64, done, total int) probeProgress {
	return probeProgress{
		Label: "confirm local sample crf " + terseFloat(crf),
		Done:  done,
		Total: total,
	}
}

func (s *crfSearch) maybeConfirmOutlier(ctx context.Context, q int, target float64, attempt ProbeAttempt) (ProbeAttempt, error) {
	if s.options.NoOutlierCheck || attempt.OutlierChecked || attempt.WorstSampleScore >= s.options.FloorVMAF || attempt.Score < target {
		return attempt, nil
	}
	idx, ok := s.singleBorderlineOutlier(attempt)
	if !ok {
		return attempt, nil
	}
	neighbors := s.outlierNeighborSamples(s.samples[idx])
	if len(neighbors) == 0 {
		return attempt, nil
	}

	// One slightly low sample can be a local VMAF oddity. Probe nearby windows
	// before forcing the entire file to a lower CRF.
	attempt.OutlierChecked = true
	attempt.OutlierScore = attempt.sampleScores[idx]
	video := buildVideoArgs(s.info, s.options.Preset, attempt.CRF)
	for i, sample := range neighbors {
		encoded, _, err := s.encodeSample(ctx, video, sample, len(s.samples)+i+1, outlierProgressScope(attempt.CRF, i*2, len(neighbors)*2))
		if err != nil {
			return attempt, err
		}
		score, err := s.scoreSample(ctx, sample, encoded, outlierProgressScope(attempt.CRF, i*2+1, len(neighbors)*2))
		if !s.options.KeepTemp {
			_ = os.Remove(encoded)
		}
		if err != nil {
			return attempt, err
		}
		attempt.OutlierNeighborScores = append(attempt.OutlierNeighborScores, score)
	}
	attempt.OutlierAccepted = allScoresAtLeast(attempt.OutlierNeighborScores, s.options.FloorVMAF)
	s.attempts[q] = attempt
	s.printOutlierAcceptedProgress(q, attempt)
	return attempt, nil
}

func (s *crfSearch) printOutlierAcceptedProgress(q int, attempt ProbeAttempt) {
	if !attempt.OutlierAccepted || s.options.Progress == nil {
		return
	}
	if s.reportedOutlierAcceptances == nil {
		s.reportedOutlierAcceptances = map[int]bool{}
	}
	if s.reportedOutlierAcceptances[q] {
		return
	}
	// The same accepted attempt can be reached from cached results, group
	// evaluation, and final selection. Print the explanation once per CRF.
	s.options.Progress.PrintLine(formatOutlierAcceptedLine(attempt))
	s.reportedOutlierAcceptances[q] = true
}

func (s *crfSearch) singleBorderlineOutlier(attempt ProbeAttempt) (int, bool) {
	if len(attempt.sampleScores) != len(s.samples) {
		return 0, false
	}
	outlier := -1
	for i, score := range attempt.sampleScores {
		if score >= s.options.FloorVMAF {
			continue
		}
		if score < s.options.FloorVMAF-outlierFloorTolerance {
			return 0, false
		}
		if outlier >= 0 {
			return 0, false
		}
		outlier = i
	}
	if outlier < 0 {
		return 0, false
	}
	return outlier, true
}

func (s *crfSearch) outlierNeighborSamples(sample SampleFile) []SampleFile {
	if sample.Duration <= 0 || s.info.Duration <= sample.Duration {
		return nil
	}
	offset := sample.Duration / 2
	maxStart := s.info.Duration - sample.Duration
	starts := []time.Duration{
		clampDuration(sample.Start-offset, 0, maxStart),
		clampDuration(sample.Start+offset, 0, maxStart),
	}
	var out []SampleFile
	for _, start := range starts {
		if start == sample.Start || sampleStartExists(out, start) {
			continue
		}
		neighbor := sample
		neighbor.Start = start
		out = append(out, neighbor)
	}
	return out
}

func allScoresAtLeast(scores []float64, floor float64) bool {
	if len(scores) == 0 {
		return false
	}
	for _, score := range scores {
		if score < floor {
			return false
		}
	}
	return true
}

func sampleStartExists(samples []SampleFile, start time.Duration) bool {
	for _, sample := range samples {
		if sample.Start == start {
			return true
		}
	}
	return false
}

func clampDuration(value, min, max time.Duration) time.Duration {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (s *crfSearch) encodeSample(ctx context.Context, video VideoArgs, sample SampleFile, sampleN int, progress probeProgress) (string, int64, error) {
	dest, err := reserveSampleTempPath(s.info.Path, s.options.TempDir, sampleN, video.CRF)
	if err != nil {
		return "", 0, err
	}
	// Track the path before ffmpeg starts so Ctrl-C or a stall can still clean
	// the current partially-written sample through Close/cleanupEncodedSamples.
	s.encodedSamplePaths = append(s.encodedSamplePaths, dest)
	committed := false
	defer func() {
		if !committed && !s.options.KeepTemp {
			_ = os.Remove(dest)
		}
	}()
	args := sampleEncodeArgs(video, sample, dest)
	if _, err := runFFmpeg(ctx, FFMpegRun{
		Role:             "sample encode",
		File:             s.info.Path,
		CRF:              video.CRF,
		ScopeLabel:       progress.Label,
		ScopeDone:        progress.Done,
		ScopeTotal:       progress.Total,
		Args:             args,
		StallTimeout:     s.options.StallTimeout,
		ExpectedDuration: sample.Duration,
		ExpectedFrames:   sample.Frames,
		Progress:         s.options.Progress,
	}); err != nil {
		return "", 0, err
	}
	stat, err := os.Stat(dest)
	if err != nil {
		return "", 0, err
	}
	if stat.Size() <= 1024 {
		return "", 0, fmt.Errorf("encode sample: encoded sample too small: %s", displayPath(dest))
	}
	committed = true
	return dest, stat.Size(), nil
}

func sampleEncodeArgs(video VideoArgs, sample SampleFile, dest string) []string {
	args := []string{
		"-y",
		"-ss", terseFloat(sample.Start.Seconds()),
		"-i", sample.SourcePath,
		"-frames:v", strconv.FormatInt(sample.Frames, 10),
		"-map", "0:v:0",
	}
	args = append(args, video.ffmpegArgs("-c:v")...)
	args = append(args, "-an", "-sn", "-dn", dest)
	return args
}

func (s *crfSearch) scoreSample(ctx context.Context, sample SampleFile, distorted string, progress probeProgress) (float64, error) {
	args := sampleScoreArgs(s.info, sample, distorted)
	res, err := runFFmpeg(ctx, FFMpegRun{
		Role:             "sample vmaf",
		File:             s.info.Path,
		ScopeLabel:       progress.Label,
		ScopeDone:        progress.Done,
		ScopeTotal:       progress.Total,
		Args:             args,
		StallTimeout:     s.options.StallTimeout,
		ExpectedDuration: sample.Duration,
		ExpectedFrames:   sample.Frames,
		ParseVMAF:        true,
		Progress:         s.options.Progress,
	})
	if err != nil {
		return 0, err
	}
	return res.VMAF, nil
}

func sampleScoreArgs(info MediaInfo, sample SampleFile, distorted string) []string {
	return []string{
		"-i", distorted,
		"-ss", terseFloat(sample.Start.Seconds()),
		"-i", sample.SourcePath,
		"-filter_complex", vmafFilter(info),
		"-frames:v", strconv.FormatInt(sample.Frames, 10),
		"-an", "-sn", "-dn",
		"-f", "null",
		nullOutputPath(),
	}
}

func (s *crfSearch) cleanupEncodedSamples() {
	if s.options.KeepTemp {
		return
	}
	for _, path := range s.encodedSamplePaths {
		_ = os.Remove(path)
	}
}

func (s *crfSearch) sortedAttempts() []ProbeAttempt {
	out := make([]ProbeAttempt, 0, len(s.attempts))
	for _, attempt := range s.attempts {
		out = append(out, attempt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CRF < out[j].CRF })
	return out
}

func qFromCRF(crf float64) int {
	return int(math.Round(crf / defaultCRFIncrement))
}

func crfFromQ(q int) float64 {
	return float64(q) * defaultCRFIncrement
}

func vmafTargets(target, floor float64) []float64 {
	if target <= 0 {
		target = defaultTargetVMAF
	}
	if floor <= 0 || floor > target {
		floor = target
	}
	var out []float64
	// Step in tenths but round each value to avoid accumulated float noise from
	// changing which effective targets are attempted.
	for v := target; v >= floor-0.0001; v -= 0.2 {
		out = append(out, math.Round(v*10)/10)
	}
	if out[len(out)-1] > floor {
		out = append(out, floor)
	}
	return out
}
