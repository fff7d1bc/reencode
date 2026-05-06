package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

var errNotVideoFile = errors.New("not a video file")
var errNoVideoStream = errors.New("no video stream found")

type MediaInfo struct {
	Path        string
	Duration    time.Duration
	FPS         float64
	TotalFrames int64
	VideoBytes  int64
	VideoIndex  int
	VideoCodec  string
	PixelFormat string
	Width       int
	Height      int
	Streams     []StreamInfo
}

type StreamInfo struct {
	Index     int
	Type      string
	CodecName string
}

func (m MediaInfo) HasVideo() bool {
	return m.VideoIndex >= 0 && m.VideoCodec != ""
}

func (m MediaInfo) IsMatroskaAV1Input() bool {
	return strings.EqualFold(extNoDot(m.Path), "mkv") && strings.EqualFold(m.VideoCodec, "av1")
}

func probeInputMedia(path string) (MediaInfo, error) {
	return probeInputMediaContext(context.Background(), path)
}

func probeInputMediaContext(ctx context.Context, path string) (MediaInfo, error) {
	ok, err := candidateVideoByContent(path)
	if err != nil {
		return MediaInfo{}, err
	}
	if !ok {
		return MediaInfo{Path: path, VideoIndex: -1}, fmt.Errorf("%s: %w", displayPath(path), errNotVideoFile)
	}
	// Eligibility checks can run several ffprobe processes in parallel. Thread
	// the command context through this path so Ctrl-C can stop the check phase
	// instead of waiting for every queued input to finish.
	return probeMediaContext(ctx, path)
}

func candidateVideoByContent(path string) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if stat.IsDir() || !stat.Mode().IsRegular() {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	var buf [512]byte
	n, err := f.Read(buf[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	// DetectContentType is a cheap stdlib prefilter, not a replacement for
	// ffprobe. If it does not report video/* we skip the file instead of probing
	// arbitrary images, text files, and sidecars from broad shell globs.
	return strings.HasPrefix(http.DetectContentType(buf[:n]), "video/"), nil
}

func probeMedia(path string) (MediaInfo, error) {
	return probeMediaContext(context.Background(), path)
}

func probeMediaContext(ctx context.Context, path string) (MediaInfo, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return MediaInfo{}, fmt.Errorf("ffprobe %s: %w", displayPath(path), ctx.Err())
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return MediaInfo{}, fmt.Errorf("ffprobe %s: %w: %s", displayPath(path), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return MediaInfo{}, fmt.Errorf("ffprobe %s: %w", displayPath(path), err)
	}

	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Index        int               `json:"index"`
			CodecType    string            `json:"codec_type"`
			CodecName    string            `json:"codec_name"`
			AvgFrameRate string            `json:"avg_frame_rate"`
			RFrameRate   string            `json:"r_frame_rate"`
			PixFmt       string            `json:"pix_fmt"`
			Width        int               `json:"width"`
			Height       int               `json:"height"`
			Tags         map[string]string `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return MediaInfo{}, fmt.Errorf("parse ffprobe json for %s: %w", displayPath(path), err)
	}

	info := MediaInfo{Path: path, VideoIndex: -1}
	if raw.Format.Duration != "" {
		seconds, err := strconv.ParseFloat(raw.Format.Duration, 64)
		if err != nil {
			return MediaInfo{}, fmt.Errorf("invalid ffprobe duration %q for %s", raw.Format.Duration, displayPath(path))
		}
		if seconds > 0 {
			info.Duration = time.Duration(seconds * float64(time.Second))
		}
	}

	for _, s := range raw.Streams {
		info.Streams = append(info.Streams, StreamInfo{
			Index:     s.Index,
			Type:      s.CodecType,
			CodecName: s.CodecName,
		})
		if info.VideoIndex < 0 && s.CodecType == "video" {
			// The first video stream is the one we encode and score. streamMapArgs
			// may still preserve other streams, but probing must stay tied to a
			// single reference stream.
			info.VideoIndex = s.Index
			info.VideoCodec = s.CodecName
			info.PixelFormat = s.PixFmt
			info.Width = s.Width
			info.Height = s.Height
			info.FPS = parseFrameRate(s.AvgFrameRate)
			if info.FPS <= 0 {
				info.FPS = parseFrameRate(s.RFrameRate)
			}
			info.VideoBytes = parseStreamByteTags(s.Tags)
		}
	}
	if !info.HasVideo() {
		return info, fmt.Errorf("%s: %w", displayPath(path), errNoVideoStream)
	}
	if info.Duration <= 0 {
		return info, fmt.Errorf("%s: missing or invalid video duration", displayPath(path))
	}
	if info.FPS <= 0 {
		return info, fmt.Errorf("%s: missing or invalid video frame rate", displayPath(path))
	}
	info.TotalFrames = estimateFrameCount(info.Duration, info.FPS)
	return info, nil
}

func estimateFrameCount(duration time.Duration, fps float64) int64 {
	if duration <= 0 || fps <= 0 {
		return 0
	}
	return int64(math.Max(1, math.Round(duration.Seconds()*fps)))
}

func parseStreamByteTags(tags map[string]string) int64 {
	for key, value := range tags {
		upper := strings.ToUpper(key)
		if upper != "NUMBER_OF_BYTES" && !strings.HasPrefix(upper, "NUMBER_OF_BYTES-") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func parseFrameRate(rate string) float64 {
	rate = strings.TrimSpace(rate)
	if rate == "" || rate == "0/0" {
		return 0
	}
	if a, b, ok := strings.Cut(rate, "/"); ok {
		num, nerr := strconv.ParseFloat(strings.TrimSpace(a), 64)
		den, derr := strconv.ParseFloat(strings.TrimSpace(b), 64)
		if nerr != nil || derr != nil || num <= 0 || den <= 0 {
			return 0
		}
		return num / den
	}
	v, err := strconv.ParseFloat(rate, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}
