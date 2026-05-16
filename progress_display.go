package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type ProgressState struct {
	Role           string
	File           string
	CRF            float64
	ScopeLabel     string
	ScopeDone      int
	ScopeTotal     int
	Frame          int64
	ExpectedFrames int64
	MediaTime      time.Duration
	Expected       time.Duration
	Speed          float64
	HaveSpeed      bool
}

type ProgressDisplay struct {
	mu            sync.Mutex
	out           io.Writer
	live          bool
	lastFlush     time.Time
	renderedLines int
	state         ProgressState
}

func NewProgressDisplay(disabled bool) *ProgressDisplay {
	if disabled || !isStderrTerminal() {
		return nil
	}
	return &ProgressDisplay{out: os.Stderr, live: true}
}

func (p *ProgressDisplay) Start(state ProgressState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
	p.flushLocked(true)
}

func (p *ProgressDisplay) Update(state ProgressState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
	if time.Since(p.lastFlush) >= time.Second {
		p.flushLocked(false)
	}
}

func (p *ProgressDisplay) Finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.renderedLines = 0
}

func (p *ProgressDisplay) PrintLine(line string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Completed probe-attempt lines must appear above the live bar, so clear the
	// old bar first and force the next progress update to redraw from scratch.
	// Keep this serialized with Start/Update/Finish. Those calls come from
	// ffmpeg reader goroutines, and interleaved ANSI clear/write sequences can
	// leave stale suffixes such as the tail of a previous CRF label or size.
	p.clearLocked()
	p.renderedLines = 0
	fmt.Fprintln(p.out, line)
	p.lastFlush = time.Time{}
}

func (p *ProgressDisplay) flushLocked(force bool) {
	if p == nil || !p.live {
		return
	}
	if !force && time.Since(p.lastFlush) < time.Second {
		return
	}
	width := terminalWidth()
	line := formatProgressLine(p.state, progressLineWidth(width))
	p.clearLocked()
	fmt.Fprint(p.out, "\x1b[2K")
	fmt.Fprintln(p.out, line)
	p.renderedLines = terminalRows(line, width)
	p.lastFlush = time.Now()
}

func (p *ProgressDisplay) clearLocked() {
	if p == nil || p.renderedLines <= 0 {
		return
	}
	// Move up, erase every rendered line, then return to the original cursor
	// position. This avoids the scrollback drift that happens when progress is
	// repeatedly printed and cleared with plain newlines.
	fmt.Fprintf(p.out, "\x1b[%dF", p.renderedLines)
	for i := 0; i < p.renderedLines; i++ {
		fmt.Fprint(p.out, "\r\x1b[2K")
		fmt.Fprintln(p.out)
	}
	fmt.Fprintf(p.out, "\x1b[%dF", p.renderedLines)
}

func formatProgressLine(state ProgressState, width int) string {
	label := state.Role
	if state.CRF > 0 {
		label += " crf " + terseFloat(state.CRF)
	}
	bar, progressText := progressParts(state, 18)
	if state.ScopeTotal > 0 {
		scopeLabel := state.ScopeLabel
		if scopeLabel == "" {
			scopeLabel = "probe"
		}
		scopeBar := progressBarForCount(state.ScopeDone, state.ScopeTotal, 18)
		scopeText := strconv.Itoa(clampInt(state.ScopeDone, 0, state.ScopeTotal)) + "/" + strconv.Itoa(state.ScopeTotal)
		activeText := progressText
		fixed := "    " + scopeLabel + " " + scopeBar + " " + scopeText + " | " + label + " " + activeText
		if state.HaveSpeed {
			fixed += " " + fmt.Sprintf("%.2fx", state.Speed)
		}
		fixed += " f=" + strconv.FormatInt(state.Frame, 10) + " "
		return fixed + truncateMiddle(displayPath(state.File), width-visibleLen(fixed))
	}
	fixed := "    " + label + " " + bar + " " + progressText
	if state.HaveSpeed {
		fixed += " " + fmt.Sprintf("%.2fx", state.Speed)
	}
	fixed += " f=" + strconv.FormatInt(state.Frame, 10) + " "
	return fixed + truncateMiddle(displayPath(state.File), width-visibleLen(fixed))
}

func displayPath(path string) string {
	if path == "" {
		return path
	}
	return filepath.Base(path)
}

func progressParts(state ProgressState, width int) (string, string) {
	if state.MediaTime > 0 && state.Expected > 0 {
		mediaRatio := progressRatio(state.MediaTime, state.Expected)
		frameRatio := progressRatioFrames(state.Frame, state.ExpectedFrames)
		if frameRatio > mediaRatio+0.05 {
			// Some ffmpeg operations report output time that lags behind decoded
			// frames. Prefer frames when the two disagree enough to make the bar
			// look stuck.
			return progressBarForFrames(state.Frame, state.ExpectedFrames, width),
				strconv.FormatInt(state.Frame, 10) + "/" + strconv.FormatInt(state.ExpectedFrames, 10) + "f"
		}
		return progressBarForDuration(state.MediaTime, state.Expected, width),
			formatDurationShort(state.MediaTime) + "/" + formatDurationMaybe(state.Expected)
	}
	if state.Frame > 0 && state.ExpectedFrames > 0 {
		return progressBarForFrames(state.Frame, state.ExpectedFrames, width),
			strconv.FormatInt(state.Frame, 10) + "/" + strconv.FormatInt(state.ExpectedFrames, 10) + "f"
	}
	current := "?"
	if state.MediaTime > 0 {
		current = formatDurationShort(state.MediaTime)
	}
	return progressBarIndeterminate(state.Frame, width), current + "/" + formatDurationMaybe(state.Expected)
}

func progressBarForDuration(done, total time.Duration, width int) string {
	if total <= 0 {
		return "[" + strings.Repeat("-", width) + "]"
	}
	ratio := progressRatio(done, total)
	return progressBarForRatio(ratio, width)
}

func progressBarForFrames(done, total int64, width int) string {
	if total <= 0 {
		return progressBarIndeterminate(done, width)
	}
	return progressBarForRatio(progressRatioFrames(done, total), width)
}

func progressBarForCount(done, total, width int) string {
	if total <= 0 {
		return progressBarIndeterminate(int64(done), width)
	}
	return progressBarForRatio(progressRatioCount(done, total), width)
}

func progressBarForRatio(ratio float64, width int) string {
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	emptyPart := strings.Repeat("-", width-filled)
	filledPart := ""
	switch {
	case filled <= 0:
	case filled >= width:
		filledPart = strings.Repeat("=", width)
		emptyPart = ""
	default:
		filledPart = strings.Repeat("=", filled-1) + ">"
	}
	return fmt.Sprintf("[%s%s] %3d%%", filledPart, emptyPart, int(ratio*100))
}

func progressBarIndeterminate(frame int64, width int) string {
	if width <= 0 {
		return "[]"
	}
	pos := 0
	if frame > 0 {
		pos = int(frame/30) % width
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < width; i++ {
		switch {
		case i == pos:
			b.WriteByte('=')
		case i == pos+1 && i < width:
			b.WriteByte('>')
		default:
			b.WriteByte('-')
		}
	}
	b.WriteString("]  ?%")
	return b.String()
}

func progressRatio(done, total time.Duration) float64 {
	if total <= 0 {
		return 0
	}
	ratio := float64(done) / float64(total)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func progressRatioFrames(done, total int64) float64 {
	if total <= 0 {
		return 0
	}
	ratio := float64(done) / float64(total)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func progressRatioCount(done, total int) float64 {
	if total <= 0 {
		return 0
	}
	ratio := float64(done) / float64(total)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func clampInt(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func formatDurationMaybe(d time.Duration) string {
	if d <= 0 {
		return "?"
	}
	return formatDurationShort(d)
}

func formatDurationShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int64(d.Round(time.Second).Seconds())
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func terminalWidth() int {
	if width := terminalWidthFromIoctl(); width > 20 {
		return width
	}
	if cols := os.Getenv("COLUMNS"); cols != "" {
		if n, err := strconv.Atoi(cols); err == nil && n > 20 {
			return n
		}
	}
	return 120
}

func progressLineWidth(width int) int {
	if width <= 1 {
		return width
	}
	// Avoid writing exactly to the last column. Many terminals set autowrap
	// after the final cell, so a visually one-line progress render may need two
	// rows to clear on the next update. Keeping one column spare makes the live
	// line cleanup deterministic.
	return width - 1
}

func terminalRows(line string, width int) int {
	if width <= 0 {
		return 1
	}
	visible := visibleLen(line)
	if visible <= 0 {
		return 1
	}
	rows := (visible + width - 1) / width
	if rows < 1 {
		return 1
	}
	return rows
}

func terminalWidthFromIoctl() int {
	type winsize struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}
	var size winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stderr.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0
	}
	return int(size.col)
}

func truncateMiddle(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return value[:left] + "..." + value[len(value)-right:]
}

func visibleLen(value string) int {
	length := 0
	inEscape := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if inEscape {
			if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') {
				inEscape = false
			}
			continue
		}
		if ch == 0x1b {
			// Progress lines contain ANSI cursor/clear sequences. Counting them
			// as visible width would truncate filenames too aggressively.
			inEscape = true
			continue
		}
		length++
	}
	return length
}

func isStderrTerminal() bool {
	stat, err := os.Stderr.Stat()
	return err == nil && (stat.Mode()&os.ModeCharDevice) != 0
}
