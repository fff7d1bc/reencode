package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepSourceCLI(t *testing.T) {
	for _, args := range [][]string{
		{"--keep-source", "input.mkv"},
		{"--keep-source", "--crf", "32", "input.mkv"},
		{"--keep-source", "--fallback-crf", "32", "input.mkv"},
		{"--keep-source", "--group-crf", "input.mkv"},
	} {
		opts, _, err := parseEncodeArgs(args)
		if err != nil || !opts.KeepSource {
			t.Fatalf("%v: keep-source=%v, %v", args, opts.KeepSource, err)
		}
	}
	opts, _, err := parseEncodeArgs([]string{"input.mkv"})
	if err != nil || opts.KeepSource {
		t.Fatalf("retention must be opt-in: %+v, %v", opts, err)
	}
}

func TestKeepSourcePublication(t *testing.T) {
	originalRun, originalInspect := executeFinalFFmpeg, inspectFinalMedia
	t.Cleanup(func() { executeFinalFFmpeg, inspectFinalMedia = originalRun, originalInspect })
	for _, mode := range []string{"success", "group", "validation", "interrupted", "changed-source"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.mkv")
			if err := os.WriteFile(path, []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			output := outputPathFor(path)
			if err := os.WriteFile(output, []byte("old output"), 0o600); err != nil {
				t.Fatal(err)
			}
			destInfo, err := os.Stat(output)
			if err != nil {
				t.Fatal(err)
			}
			input := eligibleInput{File: path, Info: validationSourceInfo(), Source: snapshotSource(before), Output: output, Destination: snapshotDestination(destInfo)}
			opts := EncodeOptions{ProbeOptions: defaultProbeOptions(), KeepSource: true, Overwrite: true, CRFSet: true, CRF: 32}
			executeFinalFFmpeg = func(_ context.Context, run FFMpegRun) (FFMpegResult, error) {
				if err := os.WriteFile(run.Args[len(run.Args)-1], []byte("new output"), 0o600); err != nil {
					return FFMpegResult{}, err
				}
				if mode == "interrupted" {
					return FFMpegResult{}, context.Canceled
				}
				if mode == "changed-source" {
					return FFMpegResult{}, os.WriteFile(path, []byte("source was changed"), 0o600)
				}
				return FFMpegResult{}, nil
			}
			inspectFinalMedia = func(context.Context, string) (MediaInfo, error) {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("source missing during validation: %v", err)
				}
				if mode == "validation" {
					return MediaInfo{}, fmt.Errorf("invalid output")
				}
				return validationOutputInfo(), nil
			}
			if mode == "group" {
				err = encodeGroup(context.Background(), opts, []eligibleInput{input})
			} else {
				err = encodeOneWithCRF(context.Background(), opts, input, 32, nil)
			}
			wantSuccess := mode == "success" || mode == "group"
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected result: %v", err)
			}
			after, err := os.Stat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("source identity changed: %v", err)
			}
			data, err := os.ReadFile(output)
			want := "old output"
			if wantSuccess {
				want = "new output"
			}
			if err != nil || string(data) != want {
				t.Fatalf("output=%q, %v", data, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					t.Errorf("leaked temporary file: %s", entry.Name())
				}
			}
		})
	}
}
