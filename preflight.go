package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	externalToolsHashOnce  sync.Once
	externalToolsHashValue string
	externalToolsHashErr   error
)

type ffmpegRequirements struct {
	SVTAV1 bool
	VMAF   bool
}

func checkFFmpegPreflight(ctx context.Context, req ffmpegRequirements) error {
	if !req.SVTAV1 && !req.VMAF {
		return nil
	}
	// Capability checks should fail quickly. They are a startup guard, not a
	// reason to hang before any actual work begins.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if req.SVTAV1 {
		out, err := ffmpegCapabilityOutput(ctx, "-encoders")
		if err != nil {
			return err
		}
		if !ffmpegListHasName(out, "libsvtav1") {
			return fmt.Errorf("ffmpeg preflight: ffmpeg does not list required encoder libsvtav1")
		}
	}
	if req.VMAF {
		out, err := ffmpegCapabilityOutput(ctx, "-filters")
		if err != nil {
			return err
		}
		if !ffmpegListHasName(out, "libvmaf") {
			return fmt.Errorf("ffmpeg preflight: ffmpeg does not list required filter libvmaf")
		}
	}
	return nil
}

func ffmpegCapabilityOutput(ctx context.Context, arg string) (string, error) {
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", arg).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("ffmpeg preflight: ffmpeg %s timed out: %w", arg, ctx.Err())
		}
		return "", fmt.Errorf("ffmpeg preflight: ffmpeg %s failed: %w", arg, err)
	}
	return string(out), nil
}

func externalToolsHash() (string, error) {
	externalToolsHashOnce.Do(func() {
		externalToolsHashValue, externalToolsHashErr = computeExternalToolsHash()
	})
	return externalToolsHashValue, externalToolsHashErr
}

func computeExternalToolsHash() (string, error) {
	// Probe cache depends on the external scoring and sample-encoding stack, not
	// just this binary. Hash command outputs that describe the ffmpeg/ffprobe
	// build plus the specific interfaces reencode relies on.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var parts []string
	for _, cmd := range [][]string{
		{"ffmpeg", "-version"},
		{"ffprobe", "-version"},
		{"ffmpeg", "-hide_banner", "-h", "encoder=libsvtav1"},
		{"ffmpeg", "-hide_banner", "-h", "filter=libvmaf"},
	} {
		out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("external tools fingerprint: %s timed out: %w", strings.Join(cmd, " "), ctx.Err())
			}
			return "", fmt.Errorf("external tools fingerprint: %s failed: %w", strings.Join(cmd, " "), err)
		}
		parts = append(parts, strings.Join(cmd, " "), string(out))
	}
	return externalToolsHashFromParts(parts), nil
}

func externalToolsHashFromParts(parts []string) string {
	h := sha512.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ffmpegListHasName(out string, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for _, field := range fields {
			// ffmpeg prints capability flags before names. Match fields exactly so
			// descriptions mentioning the name do not become false positives.
			if field == name {
				return true
			}
		}
	}
	return false
}
