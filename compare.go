package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

type CompareOptions struct {
	ProbeOptions    ProbeOptions
	Profiles        []string
	Tunes           []string
	profiles, tunes string
}

type CompareResult struct {
	Preparation ProbeTiming   `json:"preparation_timing"`
	File        string        `json:"file"`
	SamplePlan  SamplePlan    `json:"sample_plan"`
	Results     []ProbeResult `json:"results"`
	Skipped     string        `json:"skipped,omitempty"`
	Error       string        `json:"error,omitempty"`
}

func parseCompareArgs(args []string) (CompareOptions, []string, error) {
	var opts CompareOptions
	probe, files, err := parseProbeCommandArgs(args, &opts)
	if err != nil {
		return opts, nil, err
	}
	opts.ProbeOptions = probe
	opts.Profiles, err = compareList(opts.profiles, "--vmaf-profiles", []string{"legacy", "v1"})
	if err != nil {
		return opts, nil, err
	}
	opts.Tunes, err = compareList(opts.tunes, "--tunes", []string{"psnr", "vq", "vmaf"})
	return opts, files, err
}

func compareList(value, name string, allowed []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		valid := false
		for _, option := range allowed {
			if part == option {
				valid = true
			}
		}
		if !valid || seen[part] {
			return nil, fmt.Errorf("%s requires unique values from %s", name, strings.Join(allowed, ", "))
		}
		seen[part] = true
		out = append(out, part)
	}
	return out, nil
}

var compareProbe = probeMediaFile

func compareInput(ctx context.Context, opts CompareOptions, info MediaInfo) CompareResult {
	result := CompareResult{File: info.Path, SamplePlan: planSamples(info, opts.ProbeOptions.Samples, opts.ProbeOptions.SampleDuration)}
	result.Preparation = info.preparation
	info.preparation = ProbeTiming{}
	for _, profile := range opts.Profiles {
		for _, tune := range opts.Tunes {
			if ctx.Err() != nil {
				result.Error = ctx.Err().Error()
				return result
			}
			probeOpts := opts.ProbeOptions
			probeOpts.VMAFProfile, probeOpts.Tune = profile, tune
			// Every independent search receives the same inspected input and window
			// policy. Neither profile selection nor tune can alter the sample plan.
			row, session, _, err := compareProbe(ctx, probeOpts, info, false)
			if session != nil {
				session.Close()
			}
			if err != nil {
				row.Error = err.Error()
			}
			result.Results = append(result.Results, row)
		}
	}
	return result
}

func runCompareCommand(ctx context.Context, opts CompareOptions, files []string) int {
	defer commandTimer(opts.ProbeOptions)()
	if ctx.Err() != nil {
		return 130
	}
	if err := checkFFmpegPreflight(ctx, ffmpegRequirements{SVTAV1: true, VMAF: true}); err != nil {
		if ctx.Err() != nil {
			return 130
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	eligibility := collectEligibleInputs(ctx, eligibilityProbe, files, EncodeOptions{ProbeOptions: opts.ProbeOptions})
	code := 0
	enc := json.NewEncoder(os.Stdout)
	for _, entry := range eligibility.Entries {
		if ctx.Err() != nil {
			return 130
		}
		result := CompareResult{File: entry.File}
		switch {
		case entry.Skipped:
			result.Skipped = string(entry.SkipReason)
		case entry.Fatal != nil:
			result.Error = entry.Fatal.Error()
		default:
			if err := verifySourceSnapshot(entry.File, entry.Source); err != nil {
				result.Error = err.Error()
			} else {
				result = compareInput(ctx, opts, entry.Info)
			}
		}
		if ctx.Err() != nil {
			return 130
		}
		if result.Error != "" {
			code = 1
		}
		for _, row := range result.Results {
			if !row.Success {
				code = 1
			}
		}
		if opts.ProbeOptions.JSON {
			if err := enc.Encode(result); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		} else {
			printCompareHuman(os.Stdout, result)
		}
	}
	return code
}

func printCompareHuman(w io.Writer, result CompareResult) {
	fmt.Fprintln(w, displayPath(result.File)+":")
	if result.Error != "" {
		fmt.Fprintln(w, "  "+result.Error)
		return
	}
	if result.Skipped != "" {
		fmt.Fprintln(w, "  skipped: "+result.Skipped)
		return
	}
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "profile\ttune\tCRF\ttarget\tmean / worst\tpredicted\tencode / score\tmeasurement")
	for _, row := range result.Results {
		if !row.Success {
			fmt.Fprintf(table, "%s\t%s\tfailed: %s\n", row.Model.Profile, row.Tune, row.Error)
			continue
		}
		timing := fmt.Sprintf("%.2fs / %.2fs", row.Timing.EncodeSeconds, row.Timing.ScoreSeconds)
		measurement := "fresh search"
		if row.CacheHit {
			timing = "—"
			measurement = "cached"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%.2f\t%.2f / %.2f\t%s (%.0f%%)\t%s\t%s\n", row.Model.Profile, row.Tune, terseFloat(row.CRF), row.EffectiveTarget, row.Score, row.WorstSampleScore, humanBytes(row.PredictedSize), row.EncodedPercent, timing, measurement)
	}
	_ = table.Flush()
	fmt.Fprintln(w, "  shared preparation: "+result.Preparation.summary())
	for _, row := range result.Results {
		timing := row.Timing
		if row.CacheHit {
			timing.EncodeSeconds, timing.ScoreSeconds, timing.InspectionSeconds = 0, 0, 0
		}
		fmt.Fprintf(w, "  %s/%s timing: %s\n", row.Model.Profile, row.Tune, timing.summary())
		if row.Success {
			for _, sample := range row.Samples {
				fmt.Fprintf(w, "  %s/%s sample %.3fs: p05 %.2f, worst %.3fs %.2f at %.3fs\n", row.Model.Profile, row.Tune, sample.StartSeconds, sample.FrameP05, sample.WorstSecondDuration, sample.WorstSecondScore, sample.WorstSecondStart)
			}
		}
	}
}
