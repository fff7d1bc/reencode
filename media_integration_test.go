package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt in so the normal test suite does not require external media tools.
func requireMediaIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("REENCODE_INTEGRATION") != "1" {
		t.Skip("set REENCODE_INTEGRATION=1 to exercise ffmpeg and ffprobe")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
}

func mediaFixture(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create media fixture: %v\n%s", err, out)
	}
}

func TestMediaIntegrationRejectsAlpha(t *testing.T) {
	requireMediaIntegration(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "alpha.mov")
	mediaFixture(t, "-f", "lavfi", "-i", "testsrc=size=160x96:rate=24,format=argb,colorchannelmixer=aa=0.5", "-t", "3", "-c:v", "qtrle", path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, mode := range []eligibilityMode{eligibilityProbe, eligibilityEncode} {
		for _, opts := range []EncodeOptions{
			{ProbeOptions: defaultProbeOptions()},
			{CRFSet: true, CRF: 32},
			{FallbackCRFSet: true, FallbackCRF: 32},
			{ProbeOptions: ProbeOptions{ForceVideo: true}},
		} {
			result := collectEligibleInputs(ctx, mode, []string{path}, opts)
			if result.Fatal == nil || !strings.Contains(result.Fatal.Error(), "alpha") || len(result.Actionable) != 0 {
				t.Fatalf("alpha input was not rejected: %+v", result)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("source changed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected outputs: %v, %v", entries, err)
	}
}
