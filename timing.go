package main

import (
	"fmt"
	"os"
	"runtime"
	"time"
)

type ProbeTiming struct {
	MetadataSeconds   float64 `json:"metadata_seconds"`
	HDRSeconds        float64 `json:"hdr_seconds"`
	InspectionSeconds float64 `json:"inspection_seconds"`
	RuntimeSeconds    float64 `json:"runtime_seconds"`
	CacheSeconds      float64 `json:"cache_seconds"`
	EncodeSeconds     float64 `json:"encode_seconds"`
	ScoreSeconds      float64 `json:"score_seconds"`
	TotalSeconds      float64 `json:"total_seconds"`
}

func (t ProbeTiming) summary() string {
	return fmt.Sprintf("metadata %.2fs, HDR %.2fs, timestamps %.2fs, runtime %.2fs, cache %.2fs, encode %.2fs, score %.2fs, active total %.2fs",
		t.MetadataSeconds, t.HDRSeconds, t.InspectionSeconds, t.RuntimeSeconds, t.CacheSeconds, t.EncodeSeconds, t.ScoreSeconds, t.TotalSeconds)
}

func commandTimer(opts ProbeOptions) func() {
	started := time.Now()
	return func() {
		if opts.Verbose && !opts.JSON {
			fmt.Fprintf(os.Stderr, "command elapsed %.2fs (includes startup and batch waiting)\n", time.Since(started).Seconds())
		}
	}
}

func printProbePlan(opts ProbeOptions, info MediaInfo) {
	if opts.JSON || (!opts.Verbose && opts.progressDisplay() == nil) {
		return
	}
	plan := planSamples(info, opts.Samples, opts.SampleDuration)
	fmt.Fprintf(os.Stderr, "%s: probe plan %d windows, %.1fs sampled, %d inspection decoder threads\n",
		displayPath(info.Path), plan.Count, float64(plan.Count)*plan.SampleSeconds, newInspectionBudget(runtime.GOMAXPROCS(0), 1).threads)
}
