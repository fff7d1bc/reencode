package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
