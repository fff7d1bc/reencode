package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

var encoderTuneCache = struct {
	sync.Mutex
	versions map[string]string
}{versions: map[string]string{}}

var executeTuneCheck = runFFmpeg

func checkEncoderTune(ctx context.Context, opts ProbeOptions) (string, error) {
	key := opts.Tune + "/" + opts.Preset
	encoderTuneCache.Lock()
	defer encoderTuneCache.Unlock()
	if version, ok := encoderTuneCache.versions[key]; ok {
		return version, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	video := buildVideoArgs(MediaInfo{}, opts.Preset, 32, opts.Tune)
	args := []string{"-f", "lavfi", "-i", "testsrc2=s=640x360:r=30:d=0.4"}
	args = append(args, video.ffmpegArgs("-c:v")...)
	args = append(args, "-f", "null", nullOutputPath())
	res, err := executeTuneCheck(ctx, FFMpegRun{Role: "encoder tune check", Args: args, StallTimeout: 15 * time.Second})
	if err != nil {
		return "", fmt.Errorf("SVT-AV1 --tune %s check failed: %w", video.Tune, err)
	}
	version := svtVersionPattern.FindStringSubmatch(res.Stderr)
	if res.LastFrame != 12 || len(version) != 2 || !encoderReportsTune(res.Stderr, video.Tune) {
		return "", fmt.Errorf("SVT-AV1 did not confirm --tune %s or complete the test encode; upgrade ffmpeg/SVT-AV1", video.Tune)
	}
	encoderTuneCache.versions[key] = version[1]
	return version[1], nil
}

func encoderReportsTune(stderr, tune string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "preset / tune / pred struct") {
			continue
		}
		_, config, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		// The banner contains a second colon before its slash-separated values.
		config = config[strings.LastIndex(config, ":")+1:]
		fields := strings.Split(config, "/")
		if len(fields) >= 2 && strings.EqualFold(strings.TrimSpace(fields[1]), tune) {
			return true
		}
	}
	return false
}
