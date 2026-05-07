package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		printTopHelp(os.Stderr)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "help", "--help", "-h", "-help":
		printHelp(os.Args[2:])
		return
	case "probe":
		if wantsHelp(os.Args[2:]) {
			printProbeHelp(os.Stdout)
			return
		}
		opts, files, err := parseProbeArgs(os.Args[2:])
		if err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(2)
		}
		os.Exit(runProbeCommand(ctx, opts, files))
	case "encode":
		if wantsHelp(os.Args[2:]) {
			printEncodeHelp(os.Stdout)
			return
		}
		opts, files, err := parseEncodeArgs(os.Args[2:])
		if err != nil {
			if !errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(os.Stderr, err)
			}
			os.Exit(2)
		}
		os.Exit(runEncodeCommand(ctx, opts, files))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printTopHelp(os.Stderr)
		os.Exit(2)
	}
}

func parseProbeArgs(args []string) (ProbeOptions, []string, error) {
	opts := defaultProbeOptions()
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printProbeHelp(os.Stderr) }
	fs.StringVar(&opts.Preset, "preset", opts.Preset, "SVT-AV1 preset")
	fs.BoolVar(&opts.JSON, "json", false, "emit JSONL results")
	fs.Float64Var(&opts.TargetVMAF, "target-vmaf", opts.TargetVMAF, "target VMAF")
	fs.Float64Var(&opts.FloorVMAF, "vmaf-floor", opts.FloorVMAF, "lowest acceptable VMAF")
	fs.Float64Var(&opts.MaxEncodedPercent, "max-encoded-percent", opts.MaxEncodedPercent, "maximum encoded sample size percent")
	fs.BoolVar(&opts.NoOutlierCheck, "no-outlier-check", false, "disable borderline outlier confirmation")
	fs.BoolVar(&opts.NoCache, "no-cache", false, "disable probe cache")
	fs.BoolVar(&opts.RefreshCache, "refresh-cache", false, "ignore existing probe cache and write fresh result")
	fs.Var((*stringListValue)(&opts.SkipNames), "skip-name", "skip files whose basename contains this text; can be repeated")
	fs.IntVar(&opts.CheckWorkers, "check-workers", opts.CheckWorkers, "parallel eligibility check workers")
	fs.IntVar(&opts.Samples, "samples", 0, "advanced: sample count override")
	fs.StringVar(&opts.TempDir, "temp-dir", "", "advanced: temporary directory")
	fs.BoolVar(&opts.KeepTemp, "keep-temp", false, "advanced: keep temporary sample files")
	fs.BoolVar(&opts.NoProgress, "no-progress", false, "disable interactive progress display")
	fs.BoolVar(&opts.Verbose, "verbose", false, "print extra details")
	sampleDuration := durationValue{value: opts.SampleDuration}
	stallTimeout := durationValue{value: opts.StallTimeout}
	fs.Var(&sampleDuration, "sample-duration", "advanced: duration of each sample")
	fs.Var(&stallTimeout, "stall-timeout", "ffmpeg stall timeout")
	if err := fs.Parse(args); err != nil {
		return ProbeOptions{}, nil, err
	}
	if opts.CheckWorkers < 1 {
		return ProbeOptions{}, nil, fmt.Errorf("--check-workers must be at least 1")
	}
	opts.SampleDuration = sampleDuration.value
	opts.StallTimeout = stallTimeout.value
	if !opts.JSON {
		// JSON mode must keep stdout clean and deterministic. The live progress
		// display writes to stderr, but disabling it here avoids cursor control
		// sequences competing with machine-readable output in redirected runs.
		opts.Progress = NewProgressDisplay(opts.NoProgress)
	}
	if fs.NArg() == 0 {
		printProbeHelp(os.Stderr)
		return ProbeOptions{}, nil, fmt.Errorf("missing FILE")
	}
	return opts, fs.Args(), nil
}

func parseEncodeArgs(args []string) (EncodeOptions, []string, error) {
	probeOpts := defaultProbeOptions()
	opts := EncodeOptions{ProbeOptions: probeOpts}
	fs := flag.NewFlagSet("reencode", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { printEncodeHelp(os.Stderr) }
	fs.StringVar(&opts.ProbeOptions.Preset, "preset", probeOpts.Preset, "SVT-AV1 preset")
	fs.Float64Var(&opts.ProbeOptions.TargetVMAF, "target-vmaf", probeOpts.TargetVMAF, "target VMAF for probing")
	fs.Float64Var(&opts.ProbeOptions.FloorVMAF, "vmaf-floor", probeOpts.FloorVMAF, "lowest acceptable VMAF for probing")
	fs.Float64Var(&opts.ProbeOptions.MaxEncodedPercent, "max-encoded-percent", probeOpts.MaxEncodedPercent, "maximum encoded sample size percent")
	fs.BoolVar(&opts.ProbeOptions.NoOutlierCheck, "no-outlier-check", false, "disable borderline outlier confirmation")
	fs.BoolVar(&opts.ProbeOptions.NoCache, "no-cache", false, "disable probe cache")
	fs.BoolVar(&opts.ProbeOptions.RefreshCache, "refresh-cache", false, "ignore existing probe cache and write fresh result")
	fs.Var((*stringListValue)(&opts.ProbeOptions.SkipNames), "skip-name", "skip files whose basename contains this text; can be repeated")
	fs.IntVar(&opts.ProbeOptions.CheckWorkers, "check-workers", probeOpts.CheckWorkers, "parallel eligibility check workers")
	fs.IntVar(&opts.ProbeOptions.Samples, "samples", 0, "advanced: sample count override")
	fs.StringVar(&opts.ProbeOptions.TempDir, "temp-dir", "", "advanced: temporary directory")
	fs.BoolVar(&opts.ProbeOptions.KeepTemp, "keep-temp", false, "advanced: keep temporary sample files")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "print planned final ffmpeg command without encoding")
	fs.BoolVar(&opts.GroupCRF, "group-crf", false, "probe all inputs first and encode with one shared CRF")
	fs.BoolVar(&opts.ProbeOptions.NoProgress, "no-progress", false, "disable interactive progress display")
	fs.BoolVar(&opts.Overwrite, "overwrite", false, "overwrite existing output")
	fs.BoolVar(&opts.ForceReencode, "force-reencode", false, "encode even when input is already .mkv with AV1 video")
	fs.BoolVar(&opts.NoAudioTranscode, "no-audio-transcode", false, "copy all audio streams without automatic FLAC to Opus conversion")
	fs.StringVar(&opts.LogFile, "log-file", "", "write before/after size log")
	fs.BoolVar(&opts.Verbose, "verbose", false, "print extra details")
	fs.Var(&optionalFloatValue{value: &opts.CRF, set: &opts.CRFSet}, "crf", "bypass probing and encode with this CRF")
	fs.Var(&optionalFloatValue{value: &opts.FallbackCRF, set: &opts.FallbackCRFSet}, "fallback-crf", "use this CRF if probing fails")
	opts.ProbeOptions.Verbose = opts.Verbose
	sampleDuration := durationValue{value: opts.ProbeOptions.SampleDuration}
	stallTimeout := durationValue{value: opts.ProbeOptions.StallTimeout}
	fs.Var(&sampleDuration, "sample-duration", "advanced: duration of each sample")
	fs.Var(&stallTimeout, "stall-timeout", "ffmpeg stall timeout")
	if err := fs.Parse(args); err != nil {
		return EncodeOptions{}, nil, err
	}
	if opts.ProbeOptions.CheckWorkers < 1 {
		return EncodeOptions{}, nil, fmt.Errorf("--check-workers must be at least 1")
	}
	opts.ProbeOptions.SampleDuration = sampleDuration.value
	opts.ProbeOptions.StallTimeout = stallTimeout.value
	opts.ProbeOptions.Progress = NewProgressDisplay(opts.ProbeOptions.NoProgress)
	if fs.NArg() == 0 {
		printEncodeHelp(os.Stderr)
		return EncodeOptions{}, nil, fmt.Errorf("missing FILE")
	}
	return opts, fs.Args(), nil
}

type durationValue struct {
	value time.Duration
}

type optionalFloatValue struct {
	value *float64
	set   *bool
}

type stringListValue []string

func printHelp(args []string) {
	if len(args) == 0 {
		printTopHelp(os.Stdout)
		return
	}
	switch args[0] {
	case "encode":
		printEncodeHelp(os.Stdout)
	case "probe":
		printProbeHelp(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown help topic %q\n\n", args[0])
		printTopHelp(os.Stderr)
		os.Exit(2)
	}
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--help", "-h", "-help":
			return true
		}
	}
	return false
}

func (v *durationValue) String() string {
	return v.value.String()
}

func (v *durationValue) Set(s string) error {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		// Bare numbers are accepted as seconds for quick CLI use. Keep this
		// before ParseDuration so values like "20" do not become invalid.
		v.value = time.Duration(f * float64(time.Second))
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	v.value = d
	return nil
}

func (v *optionalFloatValue) String() string {
	if v == nil || v.value == nil || v.set == nil || !*v.set {
		return ""
	}
	return strconv.FormatFloat(*v.value, 'f', -1, 64)
}

func (v *optionalFloatValue) Set(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	// A separate "set" bit lets zero remain a valid explicit value instead of
	// being indistinguishable from the option not being present.
	*v.value = f
	*v.set = true
	return nil
}

func (v *stringListValue) String() string {
	if v == nil {
		return ""
	}
	return fmt.Sprint([]string(*v))
}

func (v *stringListValue) Set(s string) error {
	*v = append(*v, s)
	return nil
}
