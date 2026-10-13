package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTuneCLIArgsCacheAndMetadata(t *testing.T) {
	for tune, number := range map[string]int{"psnr": 1, "vq": 0, "vmaf": 5} {
		opts, _, err := parseEncodeArgs([]string{"--tune", tune, "--crf", "66", "movie"})
		if err != nil {
			t.Fatal(err)
		}
		video := buildVideoArgs(validationSourceInfo(), opts.ProbeOptions.Preset, 66, opts.ProbeOptions.Tune)
		for _, args := range [][]string{video.ffmpegArgs("-c:v:0"), sampleEncodeArgs(video, SampleFile{}, "out")} {
			if !strings.Contains(strings.Join(args, " "), fmt.Sprintf(":tune=%d", number)) {
				t.Fatal(args)
			}
		}
		if !strings.Contains(strings.Join(reencodeMetadataArgs(video, opts.ProbeOptions, nil), " "), "REENCODE_TUNE="+tune) {
			t.Fatal("missing tune metadata")
		}
		if got := normalizedProbeCacheOptions(opts.ProbeOptions).Tune; got != tune {
			t.Fatal(got)
		}
	}
	for _, bad := range []string{"", "5", "VMAF", "ssim"} {
		if _, _, err := parseProbeArgs([]string{"--tune", bad, "movie"}); err == nil {
			t.Fatal("invalid tune accepted", bad)
		}
	}
}

func TestTuneCheckRejectsIgnoredOption(t *testing.T) {
	original := executeTuneCheck
	defer func() { executeTuneCheck = original; encoderTuneCache.versions = map[string]string{} }()
	encoderTuneCache.versions = map[string]string{}
	opts := defaultProbeOptions()
	opts.Tune = "vmaf"
	executeTuneCheck = func(context.Context, FFMpegRun) (FFMpegResult, error) {
		return FFMpegResult{LastFrame: 12, Stderr: "SVT-AV1 Encoder Lib v4.2.0\nSvt[info]: SVT [config]: preset / tune / pred struct : 4 / PSNR / random access"}, nil
	}
	if _, err := checkEncoderTune(context.Background(), opts); err == nil {
		t.Fatal("silently ignored tune accepted")
	}
	if len(encoderTuneCache.versions) != 0 {
		t.Fatal("failed check cached")
	}
	executeTuneCheck = func(context.Context, FFMpegRun) (FFMpegResult, error) {
		return FFMpegResult{LastFrame: 12, Stderr: "SVT-AV1 Encoder Lib v4.2.0\nSvt[info]: SVT [config]: preset / tune / pred struct : 4 / VMAF / random access"}, nil
	}
	if _, err := checkEncoderTune(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
}

func TestMediaIntegrationTunes(t *testing.T) {
	requireMediaIntegration(t)
	for _, tune := range []string{"psnr", "vq", "vmaf"} {
		opts := defaultProbeOptions()
		opts.Tune = tune
		opts.Preset = "11"
		if _, err := checkEncoderTune(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
	}
}
