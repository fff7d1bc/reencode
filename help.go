package main

import (
	"fmt"
	"io"
)

func printTopHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  reencode encode [options] FILE...
  reencode probe [options] FILE...
  reencode help [command]

Commands:
  encode    Probe and encode videos to AV1 MKV outputs.
  probe     Probe videos and report selected CRF without final encoding.
  help      Show this help or command-specific help.

Examples:
  reencode encode *.mp4
  reencode encode --group-crf *.mkv
  reencode probe --json file.mkv
`)
}

func printEncodeHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  reencode encode [options] FILE...

Probe each input, encode directly to name_[e-av1].mkv, validate output, then remove the source.

Options:
  --preset string
      SVT-AV1 preset. Default: 4.
  --target-vmaf float
      Target VMAF for probing. Default: 95.
  --vmaf-floor float
      Lowest acceptable VMAF and worst-sample guardrail. Default: 94.
  --max-encoded-percent float
      Maximum encoded sample size percent. Default: 90.
  --no-outlier-check
      Disable borderline outlier confirmation.
  --no-cache
      Disable probe cache.
  --refresh-cache
      Ignore existing probe cache and write fresh result.
  --samples int
      Advanced: sample count override.
  --sample-duration duration
      Advanced: duration of each sample. Default: 20s.
  --temp-dir path
      Advanced: temporary directory.
  --keep-temp
      Advanced: keep temporary sample files.
  --dry-run
      Print the final ffmpeg command without encoding.
  --crf float
      Bypass probing and encode with this CRF.
  --fallback-crf float
      Use this CRF if probing fails.
  --group-crf
      Probe all inputs first and encode with one shared CRF.
  --no-progress
      Disable interactive progress display.
  --overwrite
      Overwrite existing output files.
  --force-reencode
      Encode even when input is already .mkv with AV1 video.
  --log-file path
      Append before/after size records.
  --stall-timeout duration
      Kill ffmpeg if frame progress stalls. Default: 10m.
  --verbose
      Print extra details.
  --help
      Show this help.

`)
}

func printProbeHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  reencode probe [options] FILE...

Probe videos and report selected CRF without final encoding.

Options:
  --preset string
      SVT-AV1 preset. Default: 4.
  --json
      Emit JSONL results.
  --target-vmaf float
      Target VMAF. Default: 95.
  --vmaf-floor float
      Lowest acceptable VMAF and worst-sample guardrail. Default: 94.
  --max-encoded-percent float
      Maximum encoded sample size percent. Default: 90.
  --no-outlier-check
      Disable borderline outlier confirmation.
  --no-cache
      Disable probe cache.
  --refresh-cache
      Ignore existing probe cache and write fresh result.
  --samples int
      Advanced: sample count override.
  --sample-duration duration
      Advanced: duration of each sample. Default: 20s.
  --temp-dir path
      Advanced: temporary directory.
  --keep-temp
      Advanced: keep temporary sample files.
  --no-progress
      Disable interactive progress display.
  --stall-timeout duration
      Kill ffmpeg if frame progress stalls. Default: 10m.
  --verbose
      Print extra details.
  --help
      Show this help.
`)
}
