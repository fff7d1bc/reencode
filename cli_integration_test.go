package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type cliHarness struct {
	t                                         *testing.T
	binary, dir, media, scratch, trace, ready string
	env                                       []string
	files                                     []string
	source                                    []byte
}

func newCLIHarness(t *testing.T) *cliHarness {
	t.Helper()
	requireMediaIntegration(t)
	binary := os.Getenv("REENCODE_TEST_BINARY")
	if binary == "" {
		t.Skip("use make test-integration to exercise the built CLI")
	}
	if runtime.GOOS == "windows" {
		t.Skip("CLI tracing shim requires a POSIX shell")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("REENCODE_TEST_BINARY must be absolute")
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatal(err)
	}
	h := &cliHarness{t: t, binary: binary, dir: t.TempDir()}
	h.media, h.scratch, h.trace, h.ready = filepath.Join(h.dir, "media"), filepath.Join(h.dir, "system-temp"), filepath.Join(h.dir, "trace"), filepath.Join(h.dir, "ready")
	bin := filepath.Join(h.dir, "bin")
	for _, dir := range []string{h.media, h.scratch, h.trace, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	shim, err := os.ReadFile("testdata/cli-tool.sh")
	if err != nil {
		t.Fatal(err)
	}
	h.env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+h.scratch, "XDG_CACHE_HOME="+filepath.Join(h.dir, "cache"),
		"REENCODE_CLI_TRACE="+h.trace, "REENCODE_CLI_READY="+h.ready, "REENCODE_CLI_CAPTURE_FAULT=")
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		real, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		h.env = append(h.env, "REENCODE_CLI_REAL_"+strings.ToUpper(tool)+"="+real)
		if err := os.WriteFile(filepath.Join(bin, tool), shim, 0700); err != nil {
			t.Fatal(err)
		}
	}
	h.files = []string{filepath.Join(h.media, "first.mkv"), filepath.Join(h.media, "second.mkv")}
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=12:duration=3", "-c:v", "ffv1", h.files[0])
	h.source, err = os.ReadFile(h.files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.files[1], h.source, 0600); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *cliHarness) command(ctx context.Context, fault string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, h.binary, args...)
	cmd.Env = append(slices.Clone(h.env), "REENCODE_CLI_CAPTURE_FAULT="+fault)
	cmd.Dir = h.media
	return cmd
}

func (h *cliHarness) run(fault string, args ...string) (string, string, int) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := h.command(ctx, fault, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		h.t.Fatalf("CLI timed out: %v\n%s", err, &stderr)
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			h.t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), stderr.String(), code
}

func (h *cliHarness) calls() [][]string {
	h.t.Helper()
	entries, err := os.ReadDir(h.trace)
	if err != nil {
		h.t.Fatal(err)
	}
	var calls [][]string
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(h.trace, entry.Name()))
		if err != nil {
			h.t.Fatal(err)
		}
		calls = append(calls, strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"))
	}
	return calls
}

func (h *cliHarness) assertClean() {
	h.t.Helper()
	for _, file := range h.files {
		if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, h.source) {
			h.t.Fatalf("source modified: %s: %v", file, err)
		}
	}
	for _, dir := range []string{h.scratch, h.media} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				h.t.Fatal("scratch leak", dir, entry.Name())
			}
		}
	}
	// Also check explicit temp directories, and paths that the OS temp helper
	// may canonicalize differently from the directory supplied by the harness.
	for _, call := range h.calls() {
		for _, arg := range call {
			if strings.HasSuffix(arg, ".crc") {
				if _, err := os.Stat(arg); !errors.Is(err, os.ErrNotExist) {
					h.t.Fatal("capture scratch survived", arg, err)
				}
			}
		}
	}
}

func decodeCLIRows[T any](t *testing.T, out string) []T {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(out))
	var rows []T
	for {
		var row T
		err := decoder.Decode(&row)
		if err == io.EOF {
			return rows
		}
		if err != nil {
			t.Fatalf("stdout is not JSONL: %v\n%s", err, out)
		}
		rows = append(rows, row)
	}
}

func TestCLIIntegrationTimingBackend(t *testing.T) {
	for _, mode := range []string{"default", "bounded", "explicit-temp", "encoder", "muxer", "timebase"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIHarness(t)
			args := []string{"probe", "--json", "--no-cache", "--preset", "11"}
			fault, policy := "", capturePolicy
			switch mode {
			case "bounded":
				args = append(args, "--samples", "1", "--sample-duration", "1s", "--no-progress")
			case "explicit-temp":
				args = append(args, "--temp-dir", filepath.Join(h.dir, "explicit-temp"), "--verbose")
			case "encoder", "muxer", "timebase":
				fault, policy = mode, inspectionPolicy
			}
			out, stderr, code := h.run(fault, append(args, h.files[0])...)
			rows := decodeCLIRows[ProbeResult](t, out)
			if code != 0 || len(rows) != 1 || !rows[0].Success || rows[0].TimingPolicy != policy {
				t.Fatalf("exit=%d rows=%+v\n%s", code, rows, stderr)
			}
			if strings.Contains(stderr, "using bounded ffprobe") != (policy == inspectionPolicy) || strings.Contains(stderr, "\x1b[") {
				t.Fatal("unexpected diagnostic or interactive output", stderr)
			}
			checks, capturedScores, hdr, extraDecodes := 0, 0, 0, 0
			for _, call := range h.calls() {
				joined := strings.Join(call, " ")
				if strings.Contains(joined, ".reencode-capture-check-") {
					checks++
				}
				if strings.Contains(joined, ".reencode-frames-") {
					capturedScores++
				}
				if call[0] == "ffprobe" && slices.Contains(call, "-show_frames") {
					if slices.Contains(call, "%+#64") {
						hdr++
					} else {
						extraDecodes++
					}
				}
			}
			if checks != 1 || hdr != 1 || (capturedScores > 0) != (policy == capturePolicy) || (extraDecodes > 0) != (policy == inspectionPolicy) {
				t.Fatalf("checks=%d scores=%d HDR=%d extra decodes=%d", checks, capturedScores, hdr, extraDecodes)
			}
			if mode == "explicit-temp" {
				entries, err := os.ReadDir(filepath.Join(h.dir, "explicit-temp"))
				if err != nil || len(entries) != 0 {
					t.Fatal("explicit temp directory was not cleaned", entries, err)
				}
			}
			h.assertClean()
		})
	}
}

func TestCLIIntegrationCaptureFaults(t *testing.T) {
	for _, fault := range []string{"unknown", "missing", "malformed", "coverage", "signal", "wait"} {
		t.Run(fault, func(t *testing.T) {
			h := newCLIHarness(t)
			out, stderr, code := h.run(fault, "probe", "--json", "--no-cache", h.files[0], h.files[1])
			rows := decodeCLIRows[ProbeResult](t, out)
			if code != 1 || len(rows) != 1 || rows[0].File != h.files[0] || rows[0].TimingPolicy != "" || !strings.Contains(rows[0].Error, "timestamp capture setup failed") {
				t.Fatalf("exit=%d rows=%+v\n%s", code, rows, stderr)
			}
			if fault == "wait" && !strings.Contains(rows[0].Error, "deadline exceeded") {
				t.Fatal(rows)
			}
			if strings.Contains(stderr, "using bounded ffprobe") {
				t.Fatal(stderr)
			}
			h.assertNoQualityWork()
			h.assertClean()
		})
	}
}

func (h *cliHarness) assertNoQualityWork() {
	h.t.Helper()
	checks := 0
	for _, call := range h.calls() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, ".reencode-capture-check-") {
			checks++
		}
		for _, unexpected := range []string{".reencode-runtime-", ".reencode-frames-", ".sample1.", ".tmp.mkv"} {
			if strings.Contains(joined, unexpected) {
				h.t.Fatal("quality work after setup fault", call)
			}
		}
		if call[0] == "ffprobe" && slices.Contains(call, "-show_frames") && !slices.Contains(call, "%+#64") {
			h.t.Fatal("compatibility decode after setup fault", call)
		}
	}
	if checks > 1 {
		h.t.Fatal("batch repeated a failed check", checks)
	}
}

func TestCLIIntegrationSetupStopsBatch(t *testing.T) {
	for _, mode := range []string{"probe", "compare", "encode", "group", "verify"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIHarness(t)
			badTemp := filepath.Join(h.dir, "not-a-directory")
			if err := os.WriteFile(badTemp, []byte("file"), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{mode, "--temp-dir", badTemp, "--no-cache", "--preset", "11"}
			if mode == "probe" || mode == "compare" {
				args = append(args, "--json", "--skip-name", "e-av1")
			} else {
				args[0] = "encode"
				args = append(args, "--overwrite", "--fallback-crf", "32")
				if mode == "group" {
					args = append(args, "--group-crf")
				}
				if mode == "verify" {
					args = append(args, "--crf", "32", "--verify-quality")
				}
			}
			for _, file := range h.files {
				if err := os.WriteFile(outputPathFor(file), []byte("existing output"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "probe" || mode == "compare" {
				args = append(args, outputPathFor(h.files[0]))
			}
			out, stderr, code := h.run("", append(args, h.files...)...)
			if code != 1 || strings.Contains(stderr, "falling back") || strings.Contains(stderr, "using --fallback-crf") || strings.Contains(stderr, "using bounded ffprobe") {
				t.Fatalf("exit=%d\n%s\n%s", code, out, stderr)
			}
			switch mode {
			case "probe":
				rows := decodeCLIRows[ProbeResult](t, out)
				if len(rows) != 2 || rows[0].File != outputPathFor(h.files[0]) || rows[0].TimingPolicy != "" || rows[1].File != h.files[0] || !strings.Contains(rows[1].Error, "timestamp capture setup failed") {
					t.Fatal(rows)
				}
			case "compare":
				rows := decodeCLIRows[CompareResult](t, out)
				if len(rows) != 2 || rows[0].Skipped == "" || rows[1].File != h.files[0] || len(rows[1].Results) != 1 || !strings.Contains(rows[1].Results[0].Error, "timestamp capture setup failed") {
					t.Fatal(rows)
				}
			default:
				if !strings.Contains(stderr, "timestamp capture setup failed") || strings.Contains(stderr, "(2 of 2)") {
					t.Fatal(stderr)
				}
			}
			for _, file := range h.files {
				if data, err := os.ReadFile(outputPathFor(file)); err != nil || string(data) != "existing output" {
					t.Fatal("output modified", file, err)
				}
			}
			h.assertNoQualityWork()
			h.assertClean()
		})
	}
}

func TestCLIIntegrationExplicitCRFBypassesCapture(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "encode", true: "dry-run"}[dryRun], func(t *testing.T) {
			h := newCLIHarness(t)
			args := []string{"encode", "--crf", "32", "--keep-source", "--preset", "11", "--no-progress"}
			if dryRun {
				args = append(args, "--dry-run", "--verify-quality")
			}
			out, stderr, code := h.run("unknown", append(args, h.files[0])...)
			if code != 0 {
				t.Fatalf("exit=%d\n%s\n%s", code, out, stderr)
			}
			for _, call := range h.calls() {
				if strings.Contains(strings.Join(call, " "), ".reencode-capture-check-") {
					t.Fatal("explicit CRF checked capture", call)
				}
			}
			_, err := os.Stat(outputPathFor(h.files[0]))
			if dryRun && !errors.Is(err, os.ErrNotExist) || !dryRun && err != nil {
				t.Fatal("wrong publication state", err)
			}
			h.assertClean()
		})
	}
}

func TestCLIIntegrationCaptureInterrupt(t *testing.T) {
	h := newCLIHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := h.command(ctx, "wait", "encode", "--overwrite", "--fallback-crf", "32", "--keep-temp", "--preset", "11", h.files[0], h.files[1])
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func(result chan<- error) { result <- cmd.Wait() }(done)
	// Always reap the CLI before t.TempDir cleanup, including failed assertions.
	defer func() {
		cancel()
		if done != nil {
			<-done
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(h.ready); err == nil {
			break
		}
		select {
		case err := <-done:
			done = nil
			t.Fatalf("CLI exited before capture: %v\n%s", err, &stderr)
		case <-ctx.Done():
			t.Fatal("capture check never started")
		case <-ticker.C:
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := <-done
	done = nil
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("interrupt exit=%v\n%s", err, &stderr)
	}
	for _, unwanted := range []string{"falling back", "using --fallback-crf", "using bounded ffprobe", "timestamp capture setup failed"} {
		if strings.Contains(stderr.String(), unwanted) {
			t.Fatal("misleading cancellation", &stderr)
		}
	}
	h.assertNoQualityWork()
	h.assertClean()
}

func TestCLIIntegrationVerboseTimingPolicy(t *testing.T) {
	h := newCLIHarness(t)
	out, stderr, code := h.run("", "probe", "--no-cache", "--preset", "11", "--verbose", "--no-progress", h.files[0])
	if code != 0 || !strings.Contains(stderr, "first.mkv: timing policy "+capturePolicy) || strings.Contains(stderr, "\x1b[") || !strings.Contains(out, "encode would use") {
		t.Fatalf("exit=%d\n%s\n%s", code, out, stderr)
	}
	h.assertClean()
}

func TestCLIIntegrationCachedTimingPolicy(t *testing.T) {
	// os.UserCacheDir uses XDG_CACHE_HOME on Linux. Do not redirect HOME or
	// touch the real user's cache on platforms without this override.
	if runtime.GOOS != "linux" {
		t.Skip("cache-isolated CLI reuse test requires XDG_CACHE_HOME")
	}
	h := newCLIHarness(t)
	for pass := range 2 {
		out, stderr, code := h.run("", "probe", "--json", "--preset", "11", h.files[0])
		rows := decodeCLIRows[ProbeResult](t, out)
		if code != 0 || len(rows) != 1 || !rows[0].Success || rows[0].CacheHit != (pass == 1) || rows[0].TimingPolicy != capturePolicy {
			t.Fatalf("pass=%d exit=%d rows=%+v\n%s", pass, code, rows, stderr)
		}
	}
	h.assertClean()
}
