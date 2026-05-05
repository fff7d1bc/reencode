package main

import (
	"math"
	"os"
	"time"
)

type SamplePlan struct {
	FullPass       bool          `json:"full_pass"`
	Count          int           `json:"count"`
	SampleDuration time.Duration `json:"-"`
	SampleSeconds  float64       `json:"sample_seconds"`
	StartsSeconds  []float64     `json:"starts_seconds"`
	Frames         int           `json:"frames"`
}

type SampleFile struct {
	SourcePath  string
	Start       time.Duration
	Duration    time.Duration
	Frames      int64
	SourceBytes int64
}

func planSamples(info MediaInfo, requested int, sampleDuration time.Duration) SamplePlan {
	if sampleDuration <= 0 {
		sampleDuration = 20 * time.Second
	}
	count := requested
	if count <= 0 {
		switch {
		case info.Duration <= 10*time.Minute:
			count = 4
		case info.Duration <= 30*time.Minute:
			count = 5
		case info.Duration <= time.Hour:
			count = 6
		default:
			count = int(math.Ceil(info.Duration.Minutes() / 10.0))
			if count > 12 {
				count = 12
			}
		}
	}
	if count < 1 {
		count = 1
	}

	if sampleDuration*time.Duration(count) >= time.Duration(float64(info.Duration)*0.85) {
		// Once samples would cover most of the source, a full pass is simpler
		// and avoids overweighting repeated sections of short files.
		return SamplePlan{
			FullPass:       true,
			Count:          1,
			SampleDuration: info.Duration,
			SampleSeconds:  info.Duration.Seconds(),
			StartsSeconds:  []float64{0},
			Frames:         int(estimateFrameCount(info.Duration, info.FPS)),
		}
	}

	frames := int(estimateFrameCount(sampleDuration, info.FPS))
	remaining := info.Duration - sampleDuration*time.Duration(count)
	gap := time.Duration(int64(remaining) / int64(count+1))
	starts := make([]float64, 0, count)
	for i := 0; i < count; i++ {
		start := gap*time.Duration(i+1) + sampleDuration*time.Duration(i)
		if sampleDuration >= 2*time.Second {
			// Whole-second starts make progress output and cache JSON stable
			// without changing the practical sample location.
			start = start.Truncate(time.Second)
		}
		starts = append(starts, start.Seconds())
	}
	return SamplePlan{
		Count:          count,
		SampleDuration: sampleDuration,
		SampleSeconds:  sampleDuration.Seconds(),
		StartsSeconds:  starts,
		Frames:         frames,
	}
}

func createSamples(info MediaInfo, plan SamplePlan) ([]SampleFile, error) {
	sourceBytes, err := sourceVideoBytes(info)
	if err != nil {
		return nil, err
	}

	if plan.FullPass {
		return []SampleFile{{
			SourcePath:  info.Path,
			Duration:    info.Duration,
			Frames:      estimateFrameCount(info.Duration, info.FPS),
			SourceBytes: sourceBytes,
		}}, nil
	}

	samples := make([]SampleFile, 0, plan.Count)
	for _, startSeconds := range plan.StartsSeconds {
		start := time.Duration(startSeconds * float64(time.Second))
		samples = append(samples, SampleFile{
			SourcePath:  info.Path,
			Start:       start,
			Duration:    plan.SampleDuration,
			Frames:      int64(plan.Frames),
			SourceBytes: estimateWindowBytes(sourceBytes, info.Duration, plan.SampleDuration),
		})
	}
	return samples, nil
}

func sourceVideoBytes(info MediaInfo) (int64, error) {
	if info.VideoBytes > 0 {
		return info.VideoBytes, nil
	}
	stat, err := os.Stat(info.Path)
	if err != nil {
		return 0, err
	}
	return stat.Size(), nil
}

func estimateWindowBytes(totalBytes int64, totalDuration, windowDuration time.Duration) int64 {
	if totalBytes <= 0 || totalDuration <= 0 || windowDuration <= 0 {
		return 0
	}
	return int64(math.Max(1, math.Round(float64(totalBytes)*(windowDuration.Seconds()/totalDuration.Seconds()))))
}
