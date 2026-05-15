package main

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

const (
	defaultVMAFPixFmt       = "yuv420p10le"
	vmafDefaultModelVersion = "vmaf_v0.6.1"
	vmaf4KModelVersion      = "vmaf_4k_v0.6.1"
)

type sampleScore struct {
	Score    float64
	Duration time.Duration
}

type scoreSummary struct {
	Mean  float64
	Worst float64
}

func summarizeScores(scores []sampleScore) scoreSummary {
	if len(scores) == 0 {
		return scoreSummary{}
	}
	var weighted float64
	var totalDuration time.Duration
	worst := scores[0].Score
	// Samples can differ in duration for full-pass and edge cases, so the
	// reported mean is duration-weighted rather than a plain average.
	for _, score := range scores {
		if score.Score < worst {
			worst = score.Score
		}
		if score.Duration > 0 {
			weighted += score.Score * score.Duration.Seconds()
			totalDuration += score.Duration
		}
	}
	if totalDuration > 0 {
		return scoreSummary{
			Mean:  weighted / totalDuration.Seconds(),
			Worst: worst,
		}
	}
	var total float64
	for _, score := range scores {
		total += score.Score
	}
	return scoreSummary{
		Mean:  total / float64(len(scores)),
		Worst: worst,
	}
}

func vmafFilter(info MediaInfo) string {
	format := "format=" + defaultVMAFPixFmt + ","
	scale := vmafScaleFilter(info.Width, info.Height)
	// Reset timestamps on both inputs. Without this, seeking source windows can
	// make libvmaf compare non-matching frames even though the decoded content
	// is otherwise correct.
	prefix := fmt.Sprintf(
		"[0:v]%s%ssetpts=PTS-STARTPTS,settb=AVTB[dis];"+
			"[1:v]%s%ssetpts=PTS-STARTPTS,settb=AVTB[ref];"+
			"[dis][ref]",
		format, scale,
		format, scale,
	)

	args := []string{
		"shortest=true",
		"ts_sync_mode=nearest",
		// Harmonic mean punishes local quality drops more than arithmetic mean,
		// which makes the CRF search less likely to hide bad samples.
		"pool=harmonic_mean",
		fmt.Sprintf("n_threads=%d", runtime.GOMAXPROCS(0)),
		// Model selection is part of the quality policy. Keep it explicit so
		// scores do not drift with ffmpeg/libvmaf defaults or package upgrades.
		"model=version=" + vmafModelVersion(info),
	}
	return prefix + "libvmaf=" + strings.Join(args, ":")
}

func vmafScaleFilter(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	if vmafUse4KModel(width, height) {
		if width < 3456 && height < 1944 {
			// VMAF models are calibrated around their target resolutions. Scale
			// only for scoring; encoded samples and final output are untouched.
			w, h := minimallyScale(width, height, 3840, 2160)
			return fmt.Sprintf("scale=%s:%s:flags=bicubic,", w, h)
		}
		return ""
	}
	if width < 1728 && height < 972 {
		// Same scoring-only upscale for low-resolution sources against the
		// default model.
		w, h := minimallyScale(width, height, 1920, 1080)
		return fmt.Sprintf("scale=%s:%s:flags=bicubic,", w, h)
	}
	return ""
}

func vmafUse4KModel(width, height int) bool {
	return width > 2560 && height > 1440
}

func vmafModelVersion(info MediaInfo) string {
	if vmafUse4KModel(info.Width, info.Height) {
		return vmaf4KModelVersion
	}
	return vmafDefaultModelVersion
}

func minimallyScale(width, height, targetWidth, targetHeight int) (string, string) {
	wFactor := float64(width) / float64(targetWidth)
	hFactor := float64(height) / float64(targetHeight)
	if hFactor > wFactor {
		return "-1", fmt.Sprintf("%d", targetHeight)
	}
	return fmt.Sprintf("%d", targetWidth), "-1"
}
