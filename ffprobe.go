package main

import (
	"context"
	"encoding/binary"
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
	framesValidated bool
	preparation     ProbeTiming
	facts           *sourceFacts
	Path            string
	Duration        time.Duration
	StartTime       time.Duration
	FPS             float64
	TotalFrames     int64
	VideoBytes      int64
	VideoIndex      int
	VideoCodec      string
	PixelFormat     string
	HasAlpha        bool
	ColorTransfer   string
	ColorPrimaries  string
	ColorSpace      string
	ColorRange      string
	HDR             string
	// Width and Height describe decoded frames after FFmpeg's autorotation,
	// so VMAF scaling and final validation use the same orientation as encoding.
	Width   int
	Height  int
	Streams []StreamInfo
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
	return probeInputMediaContextWithPolicy(ctx, path, false)
}

func probeInputMediaContextWithPolicy(ctx context.Context, path string, forceVideo bool) (MediaInfo, error) {
	info, err := probeInputMetadata(ctx, path, forceVideo)
	if err != nil {
		return info, err
	}
	return validateInputFrames(ctx, info)
}

func probeInputMetadata(ctx context.Context, path string, forceVideo bool) (info MediaInfo, err error) {
	started := time.Now()
	defer func() {
		info.preparation.MetadataSeconds = time.Since(started).Seconds()
		info.preparation.TotalSeconds = info.preparation.MetadataSeconds
	}()
	ok, err := candidateVideo(path, forceVideo)
	if err != nil {
		return MediaInfo{}, err
	}
	if !ok {
		return MediaInfo{Path: path, VideoIndex: -1}, fmt.Errorf("%s: %w", displayPath(path), errNotVideoFile)
	}
	// Eligibility checks can run several ffprobe processes in parallel. Thread
	// the command context through this path so Ctrl-C can stop the check phase
	// instead of waiting for every queued input to finish.
	stat, err := os.Stat(path)
	if err != nil {
		return MediaInfo{}, err
	}
	info, err = probeMediaContext(ctx, path)
	if err == nil {
		info.facts = &sourceFacts{source: snapshotSource(stat), timelines: make(map[sampleWindowKey]frameTimeline)}
		err = info.facts.check(path)
	}
	return info, err
}

func validateInputFrames(ctx context.Context, info MediaInfo) (result MediaInfo, retErr error) {
	info.framesValidated = false
	started := time.Now()
	var waited time.Duration
	ctx = context.WithValue(ctx, inspectionWaitKey{}, &waited)
	defer func() {
		elapsed := (time.Since(started) - waited).Seconds()
		result.preparation.HDRSeconds += elapsed
		result.preparation.TotalSeconds += elapsed
	}()
	if err := validateSourceVideo(info); err != nil {
		return info, err
	}
	if err := info.facts.check(info.Path); err != nil {
		return info, err
	}
	// Some containers expose HDR side data only after decoding. Inspect a
	// bounded prefix before accepting inputs, including explicit-CRF runs.
	var err error
	info.HDR, err = inspectEligibilityHDR(ctx, info.Path)
	if err != nil {
		return info, err
	}
	if err := validateSourceVideo(info); err != nil {
		return info, err
	}
	if err := info.facts.check(info.Path); err != nil {
		return info, err
	}
	info.framesValidated = true
	return info, nil
}

func ensureInputReady(ctx context.Context, opts ProbeOptions, info MediaInfo) (MediaInfo, error) {
	if err := ctx.Err(); err != nil {
		return info, err
	}
	if err := info.facts.check(info.Path); err != nil {
		return info, err
	}
	if info.framesValidated {
		return info, validateSourceVideo(info)
	}
	stop := opts.progressDisplay().StartActivity("input HDR checks", info.Path, 0)
	defer stop()
	return validateInputFrames(ctx, info)
}

func validateSourceVideo(info MediaInfo) error {
	// Neither our output pixel format nor VMAF preserves alpha. Reject it
	// before probing, including explicit/fallback CRF paths that bypass VMAF.
	if info.HasAlpha {
		return fmt.Errorf("%s: primary video has alpha (transparency), which cannot be preserved", displayPath(info.Path))
	}
	if info.HDR != "" {
		return fmt.Errorf("%s: primary video is HDR (%s), which is not supported", displayPath(info.Path), info.HDR)
	}
	return nil
}

type mediaSideData struct {
	Type     string   `json:"side_data_type"`
	Rotation *float64 `json:"rotation"`
}

type videoColorMetadata struct {
	ColorTransfer  string          `json:"color_transfer"`
	ColorPrimaries string          `json:"color_primaries"`
	ColorSpace     string          `json:"color_space"`
	ColorRange     string          `json:"color_range"`
	SideData       []mediaSideData `json:"side_data_list"`
}

func (m videoColorMetadata) hdrReason() string {
	switch strings.ToLower(m.ColorTransfer) {
	case "smpte2084":
		return "PQ"
	case "arib-std-b67":
		return "HLG"
	}
	for _, side := range m.SideData {
		kind := strings.ToLower(side.Type)
		if strings.Contains(kind, "dovi") || strings.Contains(kind, "dolby vision") ||
			strings.Contains(kind, "hdr") || strings.Contains(kind, "smpte2094") ||
			strings.Contains(kind, "mastering display") || strings.Contains(kind, "content light level") {
			return side.Type
		}
	}
	return ""
}

func inspectInitialHDR(ctx context.Context, path string) (string, error) {
	out, err := frameProbeOutput(ctx, 30*time.Second, "-v", "error", "-of", "json",
		"-select_streams", "v:0", "-read_intervals", "%+#64", "-show_frames",
		"-show_entries", "frame=color_transfer,color_primaries,color_space,color_range:frame_side_data=side_data_type", path)
	if ctx.Err() != nil {
		return "", fmt.Errorf("inspect initial video metadata for %s: %w", displayPath(path), ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("inspect initial video metadata for %s: %w", displayPath(path), err)
	}
	return parseInitialHDR(out)
}

func parseInitialHDR(data []byte) (string, error) {
	var raw struct {
		Frames []videoColorMetadata `json:"frames"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse initial video metadata: %w", err)
	}
	if len(raw.Frames) == 0 {
		return "", fmt.Errorf("cannot inspect initial decoded video metadata")
	}
	for _, frame := range raw.Frames {
		if reason := frame.hdrReason(); reason != "" {
			return reason, nil
		}
	}
	return "", nil
}

func candidateVideoByContent(path string) (bool, error) {
	return candidateVideo(path, false)
}

func candidateVideo(path string, forceVideo bool) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if stat.IsDir() || !stat.Mode().IsRegular() {
		return false, nil
	}
	if forceVideo {
		return true, nil
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
	// The stdlib sniffer recognizes MP4 but not QuickTime. Supplement it using
	// the same bounded read, leaving stream and metadata validation to ffprobe.
	return strings.HasPrefix(http.DetectContentType(buf[:n]), "video/") || hasQuickTimeSignature(buf[:n], stat.Size()), nil
}

func hasQuickTimeSignature(data []byte, fileSize int64) bool {
	for len(data) >= 8 && fileSize >= 8 {
		size := uint64(binary.BigEndian.Uint32(data[:4]))
		headerSize := uint64(8)
		switch size {
		case 0:
			size = uint64(fileSize)
		case 1:
			if len(data) < 16 {
				return false
			}
			size = binary.BigEndian.Uint64(data[8:16])
			headerSize = 16
		}
		if size < headerSize || size > uint64(fileSize) {
			return false
		}
		switch string(data[4:8]) {
		case "ftyp":
			if size < headerSize+8 || size > uint64(len(data)) || (size-headerSize)%4 != 0 {
				return false
			}
			for pos := headerSize; pos < size; pos += 4 {
				// The second field is a version number, not a brand.
				if pos != headerSize+4 && string(data[pos:pos+4]) == "qt  " {
					return true
				}
			}
			return false
		case "moov", "mdat":
			// Older MOV files may omit ftyp. These atoms also occur in
			// audio-only files, which ffprobe rejects later for lack of video.
			return size > headerSize
		case "wide", "free", "skip":
			// Skip leading padding only inside the sniff buffer. Never seek
			// through the movie or read an atom-sized allocation to classify it.
			if size > uint64(len(data)) {
				return false
			}
			data = data[size:]
			fileSize -= int64(size)
		default:
			return false
		}
	}
	return false
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
		"-show_pixel_formats",
		"-show_entries", "pixel_format=name:pixel_format_flags=alpha:component=",
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

	return parseMediaInfo(path, out)
}

func parseMediaInfo(path string, out []byte) (MediaInfo, error) {
	var raw struct {
		PixelFormats []struct {
			Name  string `json:"name"`
			Flags struct {
				Alpha *int `json:"alpha"`
			} `json:"flags"`
		} `json:"pixel_formats"`
		Format struct {
			Duration  string `json:"duration"`
			StartTime string `json:"start_time"`
		} `json:"format"`
		Streams []struct {
			videoColorMetadata
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
	if raw.Format.StartTime != "" && raw.Format.StartTime != "N/A" {
		start, err := timestampDuration(raw.Format.StartTime)
		if err != nil {
			return info, fmt.Errorf("invalid media start time: %w", err)
		}
		info.StartTime = start
	}
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
			// The first video stream is the one we encode and score. streamArgs
			// may still preserve other streams, but probing must stay tied to a
			// single reference stream.
			info.VideoIndex = s.Index
			info.VideoCodec = s.CodecName
			info.PixelFormat = s.PixFmt
			info.ColorTransfer = s.ColorTransfer
			info.ColorPrimaries = s.ColorPrimaries
			info.ColorSpace = s.ColorSpace
			info.ColorRange = s.ColorRange
			info.HDR = s.hdrReason()
			info.Width = s.Width
			info.Height = s.Height
			for _, side := range s.SideData {
				if side.Type == "Display Matrix" && side.Rotation != nil {
					// FFmpeg transposes near 90/270 degrees (within one degree).
					// Other angles keep the coded canvas size. Rotation metadata
					// is consumed automatically in both probe and final encodes.
					if math.Abs(math.Mod(math.Abs(*side.Rotation), 180)-90) < 1 {
						info.Width, info.Height = info.Height, info.Width
					}
					break
				}
			}
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
	alphaKnown := false
	for _, format := range raw.PixelFormats {
		if format.Name == info.PixelFormat && format.Flags.Alpha != nil {
			alphaKnown = true
			info.HasAlpha = *format.Flags.Alpha != 0
			break
		}
	}
	if !alphaKnown {
		return info, fmt.Errorf("%s: cannot determine alpha support for pixel format %q", displayPath(path), info.PixelFormat)
	}
	// Some codecs expose alpha as container side data rather than in pix_fmt
	// (notably VP8/VP9 in WebM), so also honor the stream's alpha marker.
	for _, stream := range raw.Streams {
		if stream.Index == info.VideoIndex {
			for key, value := range stream.Tags {
				if strings.EqualFold(key, "alpha_mode") && strings.TrimSpace(value) != "0" {
					info.HasAlpha = true
				}
			}
			break
		}
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
