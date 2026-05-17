package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type EncodeOptions struct {
	ProbeOptions     ProbeOptions
	DryRun           bool
	GroupCRF         bool
	Overwrite        bool
	ForceReencode    bool
	NoAudioTranscode bool
	LogFile          string
	Verbose          bool
	CRF              float64
	CRFSet           bool
	FallbackCRF      float64
	FallbackCRFSet   bool
}

func runEncodeCommand(ctx context.Context, opts EncodeOptions, files []string) int {
	if ctx.Err() != nil {
		return 130
	}
	if err := checkFFmpegPreflight(ctx, encodeFFmpegRequirements(opts)); err != nil {
		if ctx.Err() != nil {
			return 130
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	printEligibilityStart(files, false)
	eligibility := collectEligibleInputs(ctx, eligibilityEncode, files, opts)
	if eligibility.Fatal != nil {
		fmt.Fprintf(os.Stderr, "%v\n", eligibility.Fatal)
		return 1
	}
	printEligibilitySummary(eligibility, len(files), false)
	if !shouldSummarizeEligibility(files, false) {
		for _, skipped := range eligibility.Skipped {
			printSkippedInput(skipped)
		}
	}
	if opts.GroupCRF {
		if err := encodeGroup(ctx, opts, eligibility.Actionable); err != nil {
			fmt.Fprintf(os.Stderr, "group: %v\n", err)
			return 1
		}
		return 0
	}

	exitCode := 0
	for i, input := range eligibility.Actionable {
		if ctx.Err() != nil {
			return 130
		}
		file := input.File
		fmt.Fprintf(os.Stderr, ">>> Reencoding (%d of %d) %s ...\n", i+1, len(eligibility.Actionable), displayPath(file))
		if err := encodeOne(ctx, opts, file); err != nil {
			if ctx.Err() != nil {
				return 130
			}
			exitCode = 1
			fmt.Fprintf(os.Stderr, "%s: %v\n", displayPath(file), err)
		}
	}
	return exitCode
}

func encodeFFmpegRequirements(opts EncodeOptions) ffmpegRequirements {
	// An explicit-CRF dry run only prints a command, so avoid requiring local
	// ffmpeg capabilities that will not be used.
	if opts.DryRun && opts.CRFSet {
		return ffmpegRequirements{}
	}
	req := ffmpegRequirements{SVTAV1: true}
	if !opts.CRFSet {
		req.VMAF = true
	}
	return req
}

func encodeOne(ctx context.Context, opts EncodeOptions, file string) error {
	if opts.CRFSet {
		return encodeOneWithCRF(ctx, opts, file, opts.CRF, nil)
	}
	crf, probe, err := chooseCRF(ctx, opts, file)
	if err != nil {
		return err
	}
	return encodeOneWithCRF(ctx, opts, file, crf, probe)
}

func encodeOneWithCRF(ctx context.Context, opts EncodeOptions, file string, crf float64, probe *ProbeResult) error {
	info, err := probeInputMedia(file)
	if err != nil {
		return err
	}
	sourceSize := fileSize(file)
	output := outputPathFor(file)
	if _, err := os.Stat(output); err == nil && !opts.Overwrite {
		return fmt.Errorf("output already exists: %s", displayPath(output))
	}
	// Use a hidden randomized temp output. The predictable final name is only
	// claimed after validation succeeds, and the defer removes interrupted
	// encodes that would otherwise look like usable files.
	tmp, _, err := reserveFinalTempPath(file)
	if err != nil {
		return err
	}

	video := buildVideoArgs(info, opts.ProbeOptions.Preset, crf)
	mapArgs, audioArgs, dropped := streamArgs(info, opts)
	for _, stream := range dropped {
		fmt.Fprintf(os.Stderr, "%s: dropping unsupported stream %d (%s)\n", displayPath(file), stream.Index, stream.Type)
	}
	args := []string{"-y", "-i", file}
	args = append(args, mapArgs...)
	args = append(args, "-c", "copy")
	args = append(args, audioArgs...)
	args = append(args, video.ffmpegArgs("-c:v:0")...)
	args = append(args, reencodeMetadataArgs(video, opts.ProbeOptions, probe)...)
	args = append(args, tmp)

	if opts.DryRun {
		fmt.Printf("ffmpeg %s\n", strings.Join(args, " "))
		return nil
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := runFFmpeg(ctx, FFMpegRun{
		Role:             "final encode",
		File:             file,
		CRF:              crf,
		Args:             args,
		StallTimeout:     opts.ProbeOptions.StallTimeout,
		ExpectedDuration: info.Duration,
		ExpectedFrames:   info.TotalFrames,
		Progress:         opts.ProbeOptions.Progress,
	}); err != nil {
		return err
	}
	if err := validateOutput(tmp); err != nil {
		return err
	}
	if opts.Overwrite {
		_ = os.Remove(output)
	}
	if err := os.Rename(tmp, output); err != nil {
		return err
	}
	// From here on the temp path has become the final output. Do not let the
	// cleanup defer remove it if logging or source removal fails afterward.
	removeTemp = false
	if err := appendLogFile(opts.LogFile, file, output); err != nil {
		return err
	}
	outputSize := fileSize(output)
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("encoded output is ready but removing source failed: %w", err)
	}
	fmt.Fprintln(os.Stderr, formatEncodeSuccessLine(file, output, crf, sourceSize, outputSize))
	return nil
}

func formatEncodeSuccessLine(input, output string, crf float64, sourceSize, outputSize int64) string {
	return fmt.Sprintf(">>> encoded %s -> %s with crf %s, %s -> %s", displayPath(input), displayPath(output), terseFloat(crf), humanBytes(sourceSize), humanBytes(outputSize))
}

// Output metadata is written while ffmpeg creates the MKV, before the final
// file exists. Keep these tags limited to settings known up front plus probe
// scores that were already measured for the selected CRF.
type metadataTag struct {
	key   string
	value string
}

func reencodeMetadataArgs(video VideoArgs, opts ProbeOptions, probe *ProbeResult) []string {
	tags := []metadataTag{
		{"REENCODE_OUTPUT_CODEC", "av1"},
		{"REENCODE_ENCODER", video.Codec},
		{"REENCODE_PRESET", video.Preset},
		{"REENCODE_CRF", terseFloat(video.CRF)},
		{"REENCODE_PIX_FMT", video.PixFmt},
	}
	if probe != nil && probe.Success {
		target := probe.TargetVMAF
		if target == 0 {
			target = opts.TargetVMAF
		}
		floor := probe.FloorVMAF
		if floor == 0 {
			floor = opts.FloorVMAF
		}
		tags = append(tags,
			metadataTag{"REENCODE_TARGET_VMAF", terseFloat(target)},
			metadataTag{"REENCODE_VMAF_FLOOR", terseFloat(floor)},
			metadataTag{"REENCODE_AVG_VMAF", fmt.Sprintf("%.2f", probe.Score)},
			metadataTag{"REENCODE_MIN_VMAF", fmt.Sprintf("%.2f", probe.WorstSampleScore)},
		)
	}
	args := make([]string, 0, len(tags)*2)
	for _, tag := range tags {
		args = append(args, "-metadata", tag.key+"="+tag.value)
	}
	return args
}

type groupInput struct {
	File    string
	Info    MediaInfo
	Result  ProbeResult
	Session *probeSession
	Cache   *probeCacheHandle
}

func encodeGroup(ctx context.Context, opts EncodeOptions, eligible []eligibleInput) error {
	inputs := groupInputsFromEligible(eligible)
	if len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "group: no eligible files to encode")
		return nil
	}

	if opts.CRFSet {
		fmt.Fprintf(os.Stderr, "group: --crf=%s, bypassing probing for %d files\n", terseFloat(opts.CRF), len(inputs))
		return encodeGroupWithCRF(ctx, opts, inputs, opts.CRF, false)
	}

	for i := range inputs {
		fmt.Fprintf(os.Stderr, ">>> Group probing (%d of %d) %s ...\n", i+1, len(inputs), displayPath(inputs[i].File))
		result, session, cache, err := probeFileSession(ctx, opts.ProbeOptions, inputs[i].File)
		if err != nil {
			if session != nil {
				session.Close()
			}
			return groupFallbackOrError(ctx, opts, inputs, fmt.Errorf("%s: probe failed: %w", displayPath(inputs[i].File), err))
		}
		if session == nil {
			return groupFallbackOrError(ctx, opts, inputs, fmt.Errorf("%s: probe did not create a reusable session", displayPath(inputs[i].File)))
		}
		// Keep sessions open so group CRF selection can reuse cached attempts and
		// evaluate extra CRFs without rebuilding sample plans for every file.
		defer session.Close()
		inputs[i].Session = session
		inputs[i].Result = result
		inputs[i].Cache = cache
		fmt.Fprintf(os.Stderr, "%s: individual CRF %s, avg vmaf %.2f (min vmaf %.2f), %.0f%%\n",
			displayPath(inputs[i].File),
			terseFloat(result.CRF),
			result.Score,
			result.WorstSampleScore,
			result.EncodedPercent,
		)
	}

	crf, warnings, err := chooseGroupCRF(ctx, opts, inputs)
	if err != nil {
		return groupFallbackOrError(ctx, opts, inputs, err)
	}
	persistGroupProbeCaches(ctx, opts.ProbeOptions, inputs)
	fmt.Fprintf(os.Stderr, "group: shared CRF %s for %d files\n", terseFloat(crf), len(inputs))
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, warning)
	}
	return encodeGroupWithCRF(ctx, opts, inputs, crf, true)
}

func persistGroupProbeCaches(ctx context.Context, opts ProbeOptions, inputs []groupInput) {
	for _, input := range inputs {
		persistProbeSessionCache(ctx, opts, input.Cache, input.Session, input.Result)
	}
}

func persistProbeSessionCache(ctx context.Context, opts ProbeOptions, cache *probeCacheHandle, session *probeSession, result ProbeResult) {
	if ctx.Err() != nil || cache == nil || session == nil || !result.Success {
		return
	}
	// Store the expanded attempt set, not just the originally selected result.
	// Group mode and fallback paths may evaluate extra CRFs after the first
	// probe succeeds.
	result.Attempts = session.search.sortedAttempts()
	if err := storeProbeCache(cache, result); err != nil {
		warnCache(opts, "probe cache write failed: %v", err)
	}
}

func groupInputsFromEligible(eligible []eligibleInput) []groupInput {
	var inputs []groupInput
	for _, input := range eligible {
		inputs = append(inputs, groupInput{File: input.File, Info: input.Info})
	}
	return inputs
}

func chooseGroupCRF(ctx context.Context, opts EncodeOptions, inputs []groupInput) (float64, []string, error) {
	crf := initialGroupCRF(inputs)

	// Group mode favors one consistent quality level. It starts at the most
	// conservative per-file CRF and only moves downward, because higher CRF
	// values would make at least one already-probed file weaker.
	for q := qFromCRF(crf); q >= qFromCRF(defaultCRFMin); q-- {
		candidate := crfFromQ(q)
		var warnings []string
		qualityOK := true
		for _, input := range inputs {
			attempt, err := input.Session.EvaluateCRF(ctx, candidate)
			if err != nil {
				return 0, nil, err
			}
			if !groupQualityOK(attempt, opts.ProbeOptions) {
				qualityOK = false
				break
			}
			if attempt.EncodedPercent > opts.ProbeOptions.MaxEncodedPercent {
				warnings = append(warnings, fmt.Sprintf(
					"group: warning: %s passes quality at CRF %s but is %.0f%% over max %.0f%%",
					displayPath(input.File),
					terseFloat(candidate),
					attempt.EncodedPercent,
					opts.ProbeOptions.MaxEncodedPercent,
				))
			}
		}
		if qualityOK {
			return candidate, warnings, nil
		}
	}
	return 0, nil, fmt.Errorf("no shared CRF reached VMAF target %.2f and floor %.2f for every file", opts.ProbeOptions.TargetVMAF, opts.ProbeOptions.FloorVMAF)
}

func initialGroupCRF(inputs []groupInput) float64 {
	crf := inputs[0].Result.CRF
	for _, input := range inputs[1:] {
		if input.Result.CRF < crf {
			crf = input.Result.CRF
		}
	}
	return crf
}

func shouldSkipAlreadyEncoded(info MediaInfo, opts EncodeOptions) bool {
	return info.IsMatroskaAV1Input() && !opts.ForceReencode
}

func groupQualityOK(attempt ProbeAttempt, opts ProbeOptions) bool {
	return attempt.Score >= opts.TargetVMAF && (attempt.WorstSampleScore >= opts.FloorVMAF || attempt.OutlierAccepted)
}

func groupFallbackOrError(ctx context.Context, opts EncodeOptions, inputs []groupInput, probeErr error) error {
	if ctx.Err() == nil {
		// Persist partial successful probes before falling back so a retry can
		// reuse the expensive CRF attempts that already completed.
		persistGroupProbeCaches(ctx, opts.ProbeOptions, inputs)
	}
	if opts.FallbackCRFSet {
		fmt.Fprintf(os.Stderr, "group: probing failed, using --fallback-crf=%s for all files: %v\n", terseFloat(opts.FallbackCRF), probeErr)
		return encodeGroupWithCRF(ctx, opts, inputs, opts.FallbackCRF, false)
	}
	return probeErr
}

func encodeGroupWithCRF(ctx context.Context, opts EncodeOptions, inputs []groupInput, crf float64, probeMetadata bool) error {
	for i, input := range inputs {
		fmt.Fprintf(os.Stderr, ">>> Group reencoding (%d of %d) %s with CRF %s ...\n", i+1, len(inputs), displayPath(input.File), terseFloat(crf))
		var probe *ProbeResult
		if probeMetadata {
			probe = groupProbeMetadata(input, opts.ProbeOptions, crf)
		}
		if err := encodeOneWithCRF(ctx, opts, input.File, crf, probe); err != nil {
			return err
		}
	}
	return nil
}

func groupProbeMetadata(input groupInput, opts ProbeOptions, crf float64) *ProbeResult {
	if input.Session == nil {
		return nil
	}
	attempt, ok := input.Session.search.attempts[qFromCRF(crf)]
	if !ok || attempt.partial || !groupQualityOK(attempt, opts) {
		return nil
	}
	return &ProbeResult{
		File:             input.File,
		Success:          true,
		CRF:              attempt.CRF,
		TargetVMAF:       opts.TargetVMAF,
		FloorVMAF:        opts.FloorVMAF,
		Score:            attempt.Score,
		WorstSampleScore: attempt.WorstSampleScore,
		EncodedPercent:   attempt.EncodedPercent,
		PredictedSize:    attempt.PredictedSize,
		OutlierChecked:   attempt.OutlierChecked,
		OutlierAccepted:  attempt.OutlierAccepted,
		OutlierScore:     attempt.OutlierScore,
	}
}

func chooseCRF(ctx context.Context, opts EncodeOptions, file string) (float64, *ProbeResult, error) {
	result, session, cache, err := probeFileSession(ctx, opts.ProbeOptions, file)
	if session != nil {
		defer session.Close()
	}
	if err == nil && result.Success {
		return result.CRF, &result, nil
	}
	persistProbeSessionCache(ctx, opts.ProbeOptions, cache, session, result)
	if opts.FallbackCRFSet {
		fmt.Fprintf(os.Stderr, "%s: probe failed, falling back to CRF %s: %v\n", displayPath(file), terseFloat(opts.FallbackCRF), err)
		return opts.FallbackCRF, nil, nil
	}
	return 0, nil, fmt.Errorf("probe failed and --fallback-crf is not set: %w", err)
}

func streamArgs(info MediaInfo, opts EncodeOptions) ([]string, []string, []StreamInfo) {
	var mapArgs []string
	var audioArgs []string
	var dropped []StreamInfo
	audioOutputIndex := 0
	for _, stream := range info.Streams {
		switch stream.Type {
		case "video", "audio", "subtitle", "attachment":
			// Preserve streams by index rather than relying on ffmpeg defaults.
			// Defaults can silently drop extra audio, subtitles, or attachments.
			mapArgs = append(mapArgs, "-map", fmt.Sprintf("0:%d", stream.Index))
			if stream.Type == "audio" {
				if !opts.NoAudioTranscode && strings.EqualFold(stream.CodecName, "flac") {
					// ffmpeg's a:N stream specifier refers to output audio
					// order, not source stream index. Keep this counter tied to
					// mapped audio streams so FLAC overrides hit the track they
					// were derived from without changing placement.
					spec := fmt.Sprintf("a:%d", audioOutputIndex)
					audioArgs = append(audioArgs, "-c:"+spec, "libopus", "-b:"+spec, "256000")
				}
				audioOutputIndex++
			}
		default:
			dropped = append(dropped, stream)
		}
	}
	return mapArgs, audioArgs, dropped
}

func validateOutput(path string) error {
	info, err := probeMedia(path)
	if err != nil {
		return fmt.Errorf("validate output: %w", err)
	}
	if !info.HasVideo() {
		return fmt.Errorf("validate output: no video stream found")
	}
	return nil
}
