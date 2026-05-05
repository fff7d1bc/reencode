package main

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

const (
	defaultTargetVMAF        = 95.0
	defaultFloorVMAF         = 94.0
	defaultMaxEncodedPercent = 90.0
	defaultCRFMin            = 5.0
	defaultCRFMax            = 70.0
	defaultCRFIncrement      = 0.25
)

type VideoArgs struct {
	Codec  string
	Preset string
	CRF    float64
	PixFmt string
	Keyint int
	SCD    bool
}

func buildVideoArgs(info MediaInfo, preset string, crf float64) VideoArgs {
	keyint := 0
	scd := false
	if info.Duration >= 3*time.Minute && info.FPS > 0 {
		// Match probe and final encodes. Changing this only in one path makes
		// CRF decisions stop predicting the final output.
		keyint = int(math.Round(info.FPS * 10))
		scd = true
	}
	return VideoArgs{
		Codec:  "libsvtav1",
		Preset: preset,
		CRF:    crf,
		PixFmt: "yuv420p10le",
		Keyint: keyint,
		SCD:    scd,
	}
}

func (v VideoArgs) ffmpegArgs(codecOpt string) []string {
	args := []string{
		codecOpt, v.Codec,
		"-preset", v.Preset,
		"-crf", terseFloat(v.ffmpegCRF()),
		"-pix_fmt", v.PixFmt,
		"-fps_mode", "passthrough",
	}
	if v.Keyint > 0 {
		args = append(args, "-g", strconv.Itoa(v.Keyint))
	}
	scd := "0"
	if v.SCD {
		scd = "1"
	}
	args = append(args, "-svtav1-params", "scd="+scd+":crf="+terseFloat(v.CRF))
	return args
}

func (v VideoArgs) ffmpegCRF() float64 {
	if v.Codec == "libsvtav1" && v.CRF > 63 {
		// ffmpeg's libsvtav1 wrapper clamps -crf at 63, but SVT accepts higher
		// values through svtav1-params. Keep both so high-CRF probes still work.
		return 63
	}
	return v.CRF
}

func (v VideoArgs) metadata() string {
	return fmt.Sprintf("ffmpeg %s preset %s crf %s", v.Codec, v.Preset, terseFloat(v.CRF))
}

func terseFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
