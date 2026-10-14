package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Times are relative to the first decoded frame. Start is that frame's
// position on the source's playback timeline, not its container timestamp.
type frameTimeline struct {
	Start    time.Duration
	Times    []time.Duration
	Duration time.Duration
}

func timestampDuration(value string) (time.Duration, error) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || math.Abs(seconds) >= float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("invalid timestamp %q", value)
	}
	return time.Duration(math.Round(seconds * float64(time.Second))), nil
}

func inspectTimeline(ctx context.Context, path string, origin, start, duration time.Duration, full bool) (frameTimeline, error) {
	args := []string{"-v", "error", "-of", "json", "-select_streams", "v:0", "-show_frames",
		"-show_entries", "frame=best_effort_timestamp_time,duration_time,pkt_duration_time:frame_side_data="}
	if !full {
		// Seek can land before the window. Request an absolute end (not +D,
		// which ffprobe measures from the seek point), then filter by PTS.
		// Extra decode context supplies the next timestamp for the last frame.
		args = append(args, "-read_intervals", terseFloat((origin+start).Seconds())+"%"+terseFloat((origin+start+duration+2*time.Second).Seconds()))
	}
	args = append(args, "-i", path)
	out, err := frameProbeOutput(ctx, 0, args...)
	if ctx.Err() != nil {
		return frameTimeline{}, ctx.Err()
	}
	if err != nil {
		return frameTimeline{}, fmt.Errorf("inspect frame timestamps for %s: %w", displayPath(path), err)
	}
	return parseTimeline(out, origin, start, duration, full)
}

func parseTimeline(data []byte, origin, start, duration time.Duration, full bool) (frameTimeline, error) {
	var raw struct {
		Frames []struct {
			PTS            string `json:"best_effort_timestamp_time"`
			Duration       string `json:"duration_time"`
			PacketDuration string `json:"pkt_duration_time"`
		} `json:"frames"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return frameTimeline{}, fmt.Errorf("parse frame timestamps: %w", err)
	}
	var result frameTimeline
	var lastDuration time.Duration
	for _, f := range raw.Frames {
		pts, err := timestampDuration(f.PTS)
		if err != nil {
			return result, err
		}
		pts -= origin
		if !full && pts < start {
			continue
		}
		if !full && pts >= start+duration {
			if len(result.Times) > 0 {
				lastDuration = min(pts, start+duration) - result.Start - result.Times[len(result.Times)-1]
			}
			break
		}
		if len(result.Times) == 0 {
			result.Start = pts
		}
		relative := pts - result.Start
		if len(result.Times) > 0 && relative <= result.Times[len(result.Times)-1] {
			return result, fmt.Errorf("non-increasing video timestamps")
		}
		result.Times = append(result.Times, relative)
		value := f.Duration
		if value == "" || value == "N/A" {
			value = f.PacketDuration
		}
		lastDuration = 0
		if value != "" && value != "N/A" {
			lastDuration, err = timestampDuration(value)
			if err != nil {
				return result, err
			}
		}
	}
	if len(result.Times) == 0 {
		return result, fmt.Errorf("sample window has no decoded frames")
	}
	last := result.Times[len(result.Times)-1]
	if lastDuration <= 0 && len(result.Times) > 1 {
		// Some containers omit the final frame duration. Use its measured
		// predecessor interval, never the file's nominal/average frame rate.
		lastDuration = last - result.Times[len(result.Times)-2]
	}
	if !full {
		lastDuration = min(lastDuration, start+duration-result.Start-last)
	}
	if lastDuration <= 0 {
		return result, fmt.Errorf("cannot determine final frame duration")
	}
	result.Duration = last + lastDuration
	return result, nil
}

func validateTimelines(reference, distorted frameTimeline) error {
	if len(reference.Times) == 0 || len(reference.Times) != len(distorted.Times) {
		return fmt.Errorf("sample frame coverage mismatch: reference %d, encoded %d", len(reference.Times), len(distorted.Times))
	}
	// Matroska stores millisecond timestamps. Allow rounding on both sides,
	// but reject missing/repeated frames before libvmaf's nearest-frame sync.
	for i, pts := range reference.Times {
		if durationDifference(pts, distorted.Times[i]) > 2*time.Millisecond {
			return fmt.Errorf("sample timestamp mismatch at frame %d: reference %s, encoded %s", i, pts, distorted.Times[i])
		}
	}
	return nil
}

type sampleWindowKey struct {
	Start, Duration time.Duration
	Full            bool
}

func (s *crfSearch) inspectTimeline(ctx context.Context, path string, origin, start, duration time.Duration, full bool, role string) (frameTimeline, error) {
	stop := s.options.progressDisplay().StartActivity(role, s.info.Path, 0)
	defer stop()
	return inspectTimeline(ctx, path, origin, start, duration, full)
}

func (s *crfSearch) prepareSample(ctx context.Context, sample SampleFile) (SampleFile, error) {
	key := sampleWindowKey{sample.Start, sample.Duration, sample.FullPass}
	if s.timelines == nil {
		s.timelines = make(map[sampleWindowKey]frameTimeline)
	}
	timeline, ok := s.timelines[key]
	if !ok {
		var err error
		timeline, err = s.inspectTimeline(ctx, sample.SourcePath, s.info.StartTime, sample.Start, sample.Duration, sample.FullPass, "source timestamps")
		if err != nil {
			return sample, err
		}
		s.timelines[key] = timeline
	}
	sample.Timeline = &timeline
	sample.Frames = int64(len(timeline.Times))
	if !sample.FullPass {
		bytes, err := sourceVideoBytes(s.info)
		if err != nil {
			return sample, err
		}
		sample.SourceBytes = estimateWindowBytes(bytes, s.info.Duration, timeline.Duration)
	}
	return sample, nil
}

func (s SampleFile) measuredDuration() time.Duration {
	if s.Timeline != nil {
		return s.Timeline.Duration
	}
	return s.Duration
}
