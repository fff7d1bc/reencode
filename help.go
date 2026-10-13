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
  reencode help licenses

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
Detected HDR and video transparency are not supported, including with explicit CRF.

Options:
  --preset string
      SVT-AV1 preset. Default: 4.
  --tune string
      SVT-AV1 tune: psnr (default), vq, or vmaf. Requires encoder support.
  --vmaf-profile string
      VMAF profile: v1 (default, libvmaf 3.2.1+) or legacy.
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
  --force-video
      Bypass content sniffing for regular files. ffprobe must still find video.
  --skip-name text
      Skip files whose basename contains this text. Can be repeated.
  --check-workers int
      Parallel eligibility check workers. Default: 4.
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
  --keep-source
      Retain the source after successful encoding.
  --force-reencode
      Encode even when input is already .mkv with AV1 video.
  --no-audio-transcode
      Copy all audio streams without automatic FLAC to Opus conversion.
  --log-file path
      Append before/after size records.
  --stall-timeout duration
      Kill ffmpeg if frame progress stalls. Zero disables it. Default: 10m.
  --verbose
      Print extra details, including per-sample quality diagnostics.
  --help
      Show this help.

`)
}

func printProbeHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  reencode probe [options] FILE...

Probe videos and report selected CRF without final encoding.
Detected HDR and video transparency are not supported.

Options:
  --preset string
      SVT-AV1 preset. Default: 4.
  --tune string
      SVT-AV1 tune: psnr (default), vq, or vmaf. Requires encoder support.
  --vmaf-profile string
      VMAF profile: v1 (default, libvmaf 3.2.1+) or legacy.
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
  --force-video
      Bypass content sniffing for regular files. ffprobe must still find video.
  --skip-name text
      Skip files whose basename contains this text. Can be repeated.
  --check-workers int
      Parallel eligibility check workers. Default: 4.
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
      Kill ffmpeg if frame progress stalls. Zero disables it. Default: 10m.
  --verbose
      Print extra details, including per-sample quality diagnostics.
  --help
      Show this help.
`)
}
