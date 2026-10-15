package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type FFMpegRun struct {
	Role             string
	File             string
	CRF              float64
	ScopeLabel       string
	ScopeDone        int
	ScopeTotal       int
	Args             []string
	StallTimeout     time.Duration
	ExpectedDuration time.Duration
	ExpectedFrames   int64
	Progress         *ProgressDisplay
}

type FFMpegResult struct {
	Stderr        string
	LastFrame     int64
	FrameProgress bool
}

var ffmpegCommandContext = exec.CommandContext

func runFFmpeg(ctx context.Context, run FFMpegRun) (FFMpegResult, error) {
	args := append([]string{
		"-hide_banner",
		"-nostdin",
		"-stats_period", "1",
		"-progress", "pipe:1",
		"-nostats",
	}, run.Args...)
	cmd := ffmpegCommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return FFMpegResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return FFMpegResult{}, err
	}
	if run.Progress != nil {
		run.Progress.Start(ProgressState{
			Role:           run.Role,
			File:           run.File,
			CRF:            run.CRF,
			ScopeLabel:     run.ScopeLabel,
			ScopeDone:      run.ScopeDone,
			ScopeTotal:     run.ScopeTotal,
			Expected:       run.ExpectedDuration,
			ExpectedFrames: run.ExpectedFrames,
		})
		defer run.Progress.Finish()
	}
	if err := cmd.Start(); err != nil {
		return FFMpegResult{}, fmt.Errorf("%s: start ffmpeg: %w", run.Role, err)
	}

	var mu sync.Mutex
	res := FFMpegResult{LastFrame: -1}
	lastIncrease := time.Now()
	progressState := ProgressState{
		Role:           run.Role,
		File:           run.File,
		CRF:            run.CRF,
		ScopeLabel:     run.ScopeLabel,
		ScopeDone:      run.ScopeDone,
		ScopeTotal:     run.ScopeTotal,
		Expected:       run.ExpectedDuration,
		ExpectedFrames: run.ExpectedFrames,
	}
	done := make(chan struct{})
	frameAdvanced := make(chan struct{}, 1)
	var stderrBuf tailBuffer
	var readers sync.WaitGroup
	var stdoutErr, stderrErr error

	readers.Add(1)
	go func() {
		defer readers.Done()
		// Use ffmpeg's machine-readable progress stream for UI and stall
		// detection. Human stderr output changes more often and is kept only as
		// a tail for real failures.
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			update, ok := parseFFmpegProgressLine(scanner.Text())
			if !ok {
				continue
			}
			mu.Lock()
			if update.HaveFrame {
				res.FrameProgress = true
				progressState.Frame = update.Frame
				if update.Frame > res.LastFrame {
					res.LastFrame = update.Frame
					// Stall detection intentionally keys off frame movement, not
					// wall time or ffmpeg speed, because some hangs still print
					// misleading non-frame progress.
					lastIncrease = time.Now()
					select {
					case frameAdvanced <- struct{}{}:
					default:
					}
				}
			}
			if update.HaveOutTime {
				progressState.MediaTime = update.OutTime
			}
			if update.HaveSpeed {
				progressState.Speed = update.Speed
				progressState.HaveSpeed = true
			}
			state := progressState
			progress := run.Progress
			mu.Unlock()
			if progress != nil {
				progress.Update(state)
			}
		}
		if err := scanner.Err(); err != nil {
			stdoutErr = fmt.Errorf("read ffmpeg progress: %w", err)
			// A failed reader must not leave ffmpeg blocked writing to its pipe.
			_ = cmd.Process.Kill()
		}
	}()

	readers.Add(1)
	go func() {
		defer readers.Done()
		_, err := copyLines(stderr, &stderrBuf, func(string) {})
		if err != nil {
			stderrErr = fmt.Errorf("read ffmpeg stderr: %w", err)
			_ = cmd.Process.Kill()
		}
	}()

	waitErr := make(chan error, 1)
	go func() {
		// Wait closes StdoutPipe and StderrPipe. Drain both first, including
		// the final VMAF score emitted just before ffmpeg exits. Cancellation
		// and stall detection still kill the process and unblock these reads.
		readers.Wait()
		err := cmd.Wait()
		waitErr <- errors.Join(stdoutErr, stderrErr, err)
		close(done)
	}()

	var stallTimer *time.Timer
	var stallC <-chan time.Time
	if run.StallTimeout > 0 {
		stallTimer = time.NewTimer(run.StallTimeout)
		stallC = stallTimer.C
		defer stallTimer.Stop()
	}
	for {
		select {
		case err := <-waitErr:
			mu.Lock()
			res.Stderr = stderrBuf.String()
			out := res
			mu.Unlock()
			if err != nil {
				return out, ffmpegWaitError(run.Role, err, out.Stderr, ctx.Err())
			}
			return out, nil
		case <-frameAdvanced:
			mu.Lock()
			last := lastIncrease
			mu.Unlock()
			resetTimerFromLastProgress(stallTimer, run.StallTimeout, last)
		case <-stallC:
			mu.Lock()
			haveFrames := res.FrameProgress
			last := lastIncrease
			frame := res.LastFrame
			mu.Unlock()
			if remaining := run.StallTimeout - time.Since(last); remaining > 0 {
				stallTimer.Reset(remaining)
				continue
			}
			_ = cmd.Process.Kill()
			// Wait for the reader goroutines before returning so stderr is
			// drained and the terminal does not get a late dump after Ctrl-C.
			<-done
			mu.Lock()
			res.Stderr = stderrBuf.String()
			out := res
			mu.Unlock()
			if !haveFrames {
				return out, fmt.Errorf("%s: ffmpeg stalled for %s without frame progress; no frame received", run.Role, run.StallTimeout)
			}
			return out, fmt.Errorf("%s: ffmpeg stalled for %s without frame progress; last frame %d", run.Role, run.StallTimeout, frame)
		}
	}
}

func resetTimerFromLastProgress(timer *time.Timer, timeout time.Duration, last time.Time) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	remaining := timeout - time.Since(last)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	timer.Reset(remaining)
}

func ffmpegWaitError(role string, err error, stderr string, ctxErr error) error {
	if ctxErr != nil {
		return fmt.Errorf("%s: interrupted: %w", role, ctxErr)
	}
	return ffmpegFailedError(role, err, stderr)
}

func ffmpegFailedError(role string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("%s: ffmpeg failed: %w", role, err)
	}
	return fmt.Errorf("%s: ffmpeg failed: %w\n%s", role, err, stderr)
}

func copyLines(r io.Reader, dst io.Writer, onLine func(string)) (int64, error) {
	scanner := bufio.NewScanner(r)
	// ffmpeg/libvmaf can emit long filter lines. The default scanner token
	// limit is small enough to truncate useful failure context.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var n int64
	for scanner.Scan() {
		line := scanner.Text()
		n += int64(len(line) + 1)
		_, _ = io.WriteString(dst, line)
		_, _ = io.WriteString(dst, "\n")
		onLine(line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	return n, nil
}

type tailBuffer struct {
	buf bytes.Buffer
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	const max = 64 * 1024
	n, _ := b.buf.Write(p)
	if b.buf.Len() > max {
		// Keep only a tail of stderr. Full ffmpeg logs can be huge, and on
		// interrupt we intentionally omit them anyway.
		data := b.buf.Bytes()
		keep := append([]byte(nil), data[len(data)-max:]...)
		b.buf.Reset()
		_, _ = b.buf.Write(keep)
	}
	return n, nil
}

func (b *tailBuffer) String() string {
	return b.buf.String()
}

func parseProgressFrame(line string) (int64, bool) {
	update, ok := parseFFmpegProgressLine(line)
	if !ok || !update.HaveFrame {
		return 0, false
	}
	return update.Frame, true
}

type ffmpegProgressUpdate struct {
	Frame       int64
	HaveFrame   bool
	OutTime     time.Duration
	HaveOutTime bool
	Speed       float64
	HaveSpeed   bool
}

func parseFFmpegProgressLine(line string) (ffmpegProgressUpdate, bool) {
	key, value, ok := strings.Cut(line, "=")
	if !ok {
		return ffmpegProgressUpdate{}, false
	}
	value = strings.TrimSpace(value)
	switch key {
	case "frame":
		frame, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return ffmpegProgressUpdate{}, false
		}
		return ffmpegProgressUpdate{Frame: frame, HaveFrame: true}, true
	case "out_time_ms":
		// Despite the name, ffmpeg reports this progress field in microseconds.
		us, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return ffmpegProgressUpdate{}, false
		}
		return ffmpegProgressUpdate{OutTime: time.Duration(us) * time.Microsecond, HaveOutTime: true}, true
	case "out_time_us":
		us, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return ffmpegProgressUpdate{}, false
		}
		return ffmpegProgressUpdate{OutTime: time.Duration(us) * time.Microsecond, HaveOutTime: true}, true
	case "speed":
		speedText := strings.TrimSuffix(value, "x")
		speed, err := strconv.ParseFloat(speedText, 64)
		if err != nil {
			return ffmpegProgressUpdate{}, false
		}
		return ffmpegProgressUpdate{Speed: speed, HaveSpeed: true}, true
	default:
		return ffmpegProgressUpdate{}, false
	}
}
