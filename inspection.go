package main

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

type inspectionBudgetKey struct{}
type inspectionWaitKey struct{}

type inspectionBudget struct {
	slots   chan struct{}
	threads int
}

func newInspectionBudget(cpus, workers int) *inspectionBudget {
	// This is a decoder-thread budget, not an OS CPU-usage limit. Divide it
	// across concurrent checks instead of granting a quarter of the CPUs to
	// every eligibility worker. Metadata-only probes do not occupy a slot.
	total := max(1, cpus/4)
	concurrent := min(max(1, workers), total)
	return &inspectionBudget{slots: make(chan struct{}, concurrent), threads: total / concurrent}
}

var inspectionCommandContext = exec.CommandContext

func frameProbeOutput(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	budget, _ := ctx.Value(inspectionBudgetKey{}).(*inspectionBudget)
	if budget == nil {
		budget = newInspectionBudget(runtime.GOMAXPROCS(0), 1)
	}
	waitStarted := time.Now()
	select {
	case budget.slots <- struct{}{}:
		defer func() { <-budget.slots }()
	case <-ctx.Done():
		if waited, ok := ctx.Value(inspectionWaitKey{}).(*time.Duration); ok {
			*waited += time.Since(waitStarted)
		}
		return nil, ctx.Err()
	}
	if waited, ok := ctx.Value(inspectionWaitKey{}).(*time.Duration); ok {
		*waited += time.Since(waitStarted)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Queueing behind other checks must not consume the subprocess timeout.
	// The parent context still cancels both waiting and active inspections.
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// ffprobe defaults to one decoder thread. -show_frames performs real
	// decoding, unlike a metadata query, so set the bounded budget explicitly.
	args = append([]string{"-threads", strconv.Itoa(budget.threads)}, args...)
	out, err := inspectionCommandContext(ctx, "ffprobe", args...).Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}
