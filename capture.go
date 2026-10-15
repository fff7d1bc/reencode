package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const capturePolicy = "decoded-framecrc-v1"
const inspectionPolicy = "decoded-ffprobe-v1"

// wrapped_avframe passes AVFrame references, not serialized pixels. framecrc
// exposes their integer timestamps without another decode or a raw-video file.
// CRC values describe the wrapper and are deliberately ignored.
func captureOutputArgs(path string) []string {
	return []string{"-map", "[capture_dis]", "-map", "[capture_ref]", "-c:v", "wrapped_avframe",
		"-enc_time_base:v", "filter", "-fps_mode", "passthrough", "-f", "framecrc", path}
}

func captureFilter(filter string, duration time.Duration, full bool) string {
	prefix := "[0:v:0]split[capture_dis][score_dis];[1:v:0]split[capture_ref][source_ref];"
	if full {
		prefix += "[source_ref]null[score_ref];"
	} else {
		// Keep the next source frame as duration context, but never score it.
		prefix += "[source_ref]trim=end=" + terseFloat(duration.Seconds()) + "[score_ref];"
	}
	filter = strings.Replace(filter, "[0:v]", "[score_dis]", 1)
	filter = strings.Replace(filter, "[1:v]", "[score_ref]", 1)
	return prefix + filter + "[quality]"
}

func readFrameCapture(r io.Reader) ([2][]decodedFrame, error) {
	var frames [2][]decodedFrame
	var bases [2]*big.Rat
	var codecs, media [2]bool
	scanner := bufio.NewScanner(r)
	// Side-data suffixes are ignored, but still bounded to catch corrupt output.
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			for _, name := range []string{"tb", "codec_id", "media_type"} {
				prefix := "#" + name + " "
				if !strings.HasPrefix(line, prefix) {
					continue
				}
				parts := strings.SplitN(strings.TrimPrefix(line, prefix), ":", 2)
				if len(parts) != 2 {
					return frames, fmt.Errorf("malformed capture header")
				}
				i, err := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err != nil || i < 0 || i >= len(frames) {
					return frames, fmt.Errorf("unexpected capture stream")
				}
				value := strings.TrimSpace(parts[1])
				switch name {
				case "tb":
					base, ok := new(big.Rat).SetString(value)
					if !ok || base.Sign() <= 0 || bases[i] != nil {
						return frames, fmt.Errorf("invalid capture timebase")
					}
					bases[i] = base
				case "codec_id":
					if codecs[i] || value != "wrapped_avframe" {
						return frames, fmt.Errorf("invalid capture codec")
					}
					codecs[i] = true
				case "media_type":
					if media[i] || value != "video" {
						return frames, fmt.Errorf("invalid capture media type")
					}
					media[i] = true
				}
			}
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			return frames, fmt.Errorf("truncated capture frame")
		}
		var values [4]int64
		for j := range values {
			n, err := strconv.ParseInt(strings.TrimSpace(parts[j]), 10, 64)
			if err != nil {
				return frames, fmt.Errorf("invalid capture timestamp: %w", err)
			}
			values[j] = n
		}
		i := values[0]
		if i < 0 || i >= 2 || bases[i] == nil || !codecs[i] || !media[i] || values[3] < 0 {
			return frames, fmt.Errorf("capture frame lacks valid stream identity")
		}
		pts, err := captureTimestamp(values[2], bases[i])
		if err != nil {
			return frames, err
		}
		duration, err := captureTimestamp(values[3], bases[i])
		if err != nil {
			return frames, err
		}
		if n := len(frames[i]); n > 0 && pts <= frames[i][n-1].PTS {
			return frames, fmt.Errorf("non-increasing capture timestamps")
		}
		frames[i] = append(frames[i], decodedFrame{pts, duration})
	}
	if err := scanner.Err(); err != nil {
		return frames, err
	}
	for i := range frames {
		if len(frames[i]) == 0 {
			return frames, fmt.Errorf("missing captured stream %d", i)
		}
	}
	return frames, nil
}

func captureTimestamp(value int64, base *big.Rat) (time.Duration, error) {
	n := new(big.Int).Mul(big.NewInt(value), base.Num())
	n.Mul(n, big.NewInt(int64(time.Second)))
	// Round rational timestamps to nanoseconds without float precision loss.
	abs := new(big.Int).Abs(n)
	abs.Add(abs, new(big.Int).Quo(base.Denom(), big.NewInt(2)))
	abs.Quo(abs, base.Denom())
	if n.Sign() < 0 {
		abs.Neg(abs)
	}
	if !abs.IsInt64() {
		return 0, fmt.Errorf("capture timestamp overflow")
	}
	return time.Duration(abs.Int64()), nil
}

func loadFrameCapture(path string) ([2][]decodedFrame, error) {
	f, err := os.Open(path)
	if err != nil {
		return [2][]decodedFrame{}, fmt.Errorf("read frame capture: %w", err)
	}
	defer f.Close()
	return readFrameCapture(f)
}

var captureSupport struct {
	sync.Mutex
	checked bool
	backend string
}

var checkCaptureSupport = selectCaptureBackend
var checkFrameCapture = smokeFrameCapture

// Only an identified missing capture feature permits the compatibility path.
// Setup faults are command-wide failures, not failed quality searches eligible
// for --fallback-crf. Keep their cause intact across command-level wrapping.
type captureUnsupportedError struct{ reason string }

func (e *captureUnsupportedError) Error() string { return e.reason }

type captureSetupError struct{ err error }

func (e *captureSetupError) Error() string { return "timestamp capture setup failed: " + e.err.Error() }
func (e *captureSetupError) Unwrap() error { return e.err }

func isCaptureSetupError(err error) bool {
	var setup *captureSetupError
	return errors.As(err, &setup)
}

// An exit accompanied by a pipe/read failure is not reliable capability
// evidence. Neither a failed start nor a signal has an ordinary exit code.
func ordinaryExitOnly(err error) bool {
	switch e := err.(type) {
	case *exec.ExitError:
		return e.ExitCode() > 0
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		return len(causes) == 1 && ordinaryExitOnly(causes[0])
	case interface{ Unwrap() error }:
		return ordinaryExitOnly(e.Unwrap())
	default:
		return false
	}
}

func unsupportedCaptureFeature(err error, stderr string) error {
	if !ordinaryExitOnly(err) {
		return nil
	}
	// Do not match generic "Invalid argument" or unrelated encoder/filter
	// failures. Unknown diagnostics must remain visible as setup errors.
	for _, diagnostic := range []string{
		"unknown encoder 'wrapped_avframe'",
		"encoder (codec wrapped_avframe) not found",
		"requested output format 'framecrc' is not known",
		"unknown format 'framecrc'",
		"invalid time base: filter",
	} {
		if strings.Contains(strings.ToLower(stderr), diagnostic) {
			return &captureUnsupportedError{reason: diagnostic}
		}
	}
	return nil
}

func selectCaptureBackend(ctx context.Context, opts ProbeOptions) (string, error) {
	captureSupport.Lock()
	defer captureSupport.Unlock()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if captureSupport.checked {
		return captureSupport.backend, nil
	}
	err := checkFrameCapture(ctx, opts)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	backend := capturePolicy
	if err != nil {
		var unsupported *captureUnsupportedError
		if !errors.As(err, &unsupported) {
			return "", &captureSetupError{err: err}
		}
		backend = inspectionPolicy
		opts.progressDisplay().Printf("decoded timestamp capture unavailable, using bounded ffprobe inspection: %v\n", err)
	}
	captureSupport.backend = backend
	captureSupport.checked = true
	return captureSupport.backend, nil
}

func smokeFrameCapture(ctx context.Context, opts ProbeOptions) error {
	dir := opts.TempDir
	if dir == "" {
		// Capability checks have no source directory. Match the scoring runtime
		// check's system-temp default instead of passing an empty path to MkdirAll.
		dir = os.TempDir()
	}
	path, err := reserveTempPath(dir, ".reencode-capture-check-*.crc")
	if err != nil {
		return err
	}
	defer os.Remove(path)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Unequal lengths are essential: a shortest=true consumer must not truncate
	// observation of the longer input and hide an extra decoded frame.
	filter := captureFilter("[0:v]settb=AVTB,setpts=N[d];[1:v]settb=AVTB,setpts=N[r];[d][r]libvmaf=shortest=true:n_threads=1", 0, true)
	args := []string{"-y", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24:duration=0.125",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=24:duration=0.083333",
		"-filter_complex", filter, "-map", "[quality]", "-fps_mode", "passthrough", "-f", "null", nullOutputPath()}
	args = append(args, captureOutputArgs(path)...)
	if run, err := runFFmpeg(ctx, FFMpegRun{Role: "timestamp capture check", Args: args, Progress: opts.progressDisplay()}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if unsupported := unsupportedCaptureFeature(err, run.Stderr); unsupported != nil {
			return unsupported
		}
		return err
	}
	frames, err := loadFrameCapture(path)
	if err != nil {
		return err
	}
	if len(frames[0]) != 3 || len(frames[1]) != 2 || frames[0][2].PTS != time.Second/12 || frames[1][1].Duration != time.Duration(41666667) {
		return fmt.Errorf("timestamp capture failed unequal-stream coverage check")
	}
	return nil
}
