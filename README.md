# reencode

`reencode` is a command-line tool for unattended AV1 reencoding. It is
designed for the boring but useful job of walking over video files, choosing a
reasonable quality-preserving CRF automatically, writing compact MKV outputs,
validating them, and only then removing the source file.

The default goal is not to chase the smallest possible file at any cost. The
goal is to save space without visibly damaging the video, using VMAF-guided
probing and conservative quality guardrails.

`reencode` uses `ffmpeg` for media work, `ffprobe` for inspection, and
`libsvtav1` as the AV1 encoder. It does not link against ffmpeg libraries.

## Why Use It

`reencode` is meant for workflows where a wrapper script should not be needed:

- run it over directories or shell globs and leave it alone
- skip non-video files caught by broad globs such as `path/to/dir/*`
- skip filenames you opt out with repeated `--skip-name TEXT` filters
- automatically choose CRF per file instead of hand-tuning each encode
- use one shared CRF for related files, such as episodes from the same disc
- reuse cached probe work when a file is probed again
- stop stalled ffmpeg jobs instead of waiting forever
- keep stdout usable for JSON probe output and write progress/status to stderr.

## Quick Start

```sh
reencode encode *.mp4
```

For each eligible input, `reencode` probes quality, selects a CRF, encodes
directly from the original file to an MKV output, validates the result, and then
removes the source.

Final encode keeps source stream order and copies non-video streams by default.
FLAC audio streams are converted to Opus at `256000` bps to save space while
other audio codecs are copied. Use `--no-audio-transcode` to copy all audio
streams unchanged.

Outputs are written next to the input:

```text
name_[e-av1].mkv
```

Inputs are skipped when built-in content sniffing does not identify them as
video files. Inputs are also skipped when they are already `.mkv` files whose
primary video codec is AV1. Use `--force-reencode` to bypass the already-AV1
skip. Use `--skip-name TEXT` to skip inputs whose basename contains a marker
you choose, such as `[reencoded]`. The option can be repeated.

When more than one input is passed, `reencode` checks eligibility first and
prints a compact summary before starting probe or encode work. The work counter
then counts only actionable files, not every path matched by the shell. This
check runs up to four files in parallel by default. Use `--check-workers N` to
adjust it.

Useful encode examples:

```sh
reencode encode *.mp4
reencode encode --group-crf *.mkv
reencode encode --fallback-crf 32 *.mkv
reencode encode --skip-name '[reencoded]' --skip-name '[reencoded-av1]' *
reencode encode --dry-run --crf 28 file.mkv
```

## How Probing Works

The `probe` subcommand shows the CRF decision without doing a final encode:

```sh
reencode probe file.mkv
reencode probe --json file.mkv
```

For each file, probing does this:

1. Inspect duration, frame rate, stream layout, codec, pixel format, resolution,
   and video stream size with `ffprobe`.
2. Choose sample windows from across the source. The default sample duration is
   `20s`. The sample count is chosen automatically from the input duration
   unless `--samples` is set.
3. Encode each sample window with the same SVT-AV1 settings used for the final
   encode, including preset, CRF, pixel format, keyframe interval, and SVT
   parameters.
4. Score each encoded sample against the matching source window with ffmpeg
   `libvmaf`.
5. Search CRF values and select the highest CRF that still satisfies the
   quality rules and sample-size cap.

The VMAF scoring path is intentionally conservative:

- timestamps are normalized before scoring
- source and encoded samples use a shared pixel format
- VMAF uses `pool=harmonic_mean`
- VMAF model selection is explicit and deterministic
- normal sources use `vmaf_v0.6.1`
- 4K-oriented sources use `vmaf_4k_v0.6.1`
- low-resolution sources may be scaled for VMAF only
- the final decision uses both a weighted mean score and a worst-sample floor.

`reencode` does not try to discover and use the newest installed VMAF model at
runtime. Model choice changes the score, so automatic discovery would make
results vary across machines and package upgrades. The selected model is part
of the probe cache identity.

## Target VMAF And Floor

`--target-vmaf` and `--vmaf-floor` are separate checks.

The reported `VMAF` is the duration-weighted mean across all probe samples. It
must meet `--target-vmaf`, which defaults to `95`.

The reported `worst` is the lowest individual sample score. It must meet
`--vmaf-floor`, which defaults to `94`.

Examples:

- `VMAF 95.2, worst 94.4`: pass
- `VMAF 95.2, worst 91.0`: fail, at least one section is too damaged
- `VMAF 94.8, worst 94.5`: fail, samples are consistent but the average quality
  is below target.

If exactly one sample misses the floor by less than `0.75` VMAF while the mean
score and size cap pass, `reencode` checks two nearby windows at the same CRF.
The CRF is accepted only when both nearby windows pass the floor. This avoids
lowering CRF for the whole file because of one local borderline reading. Use
`--no-outlier-check` to disable this confirmation.

## Commands

### `encode`

```sh
reencode encode [options] FILE...
```

Probe each eligible input, encode it to `name_[e-av1].mkv`, validate the output,
and remove the source after validation succeeds.

Common options:

- `--preset N`: use a different SVT-AV1 preset. Default: `4`.
- `--crf N`: bypass probing and encode with this CRF.
- `--fallback-crf N`: use this CRF when probing fails.
- `--group-crf`: probe all inputs first and encode with one shared CRF.
- `--overwrite`: allow replacing an existing output file.
- `--force-reencode`: encode even when the input is already `.mkv` with AV1
  video.
- `--no-audio-transcode`: copy all audio streams without converting FLAC to
  Opus.
- `--skip-name TEXT`: skip files whose basename contains this text. Repeat the
  option for multiple markers.
- `--check-workers N`: set parallel eligibility check workers. Default: `4`.
- `--dry-run`: print the final ffmpeg command without encoding.
- `--log-file PATH`: append before/after size records.
- `--no-progress`: disable the interactive progress display.

### `probe`

```sh
reencode probe [options] FILE...
```

Probe files and report the selected CRF without final encoding. With `--json`,
results are written as JSONL on stdout.

Probe options shared by `probe` and `encode`:

- `--target-vmaf N`: weighted mean VMAF target. Default: `95`.
- `--vmaf-floor N`: worst-sample floor. Default: `94`.
- `--max-encoded-percent N`: maximum encoded sample size percent. Default: `90`.
- `--samples N`: override automatic sample count.
- `--sample-duration DURATION`: duration of each sample. Default: `20s`.
- `--no-outlier-check`: disable nearby-window confirmation for one borderline
  low sample.
- `--no-cache`: bypass probe cache.
- `--refresh-cache`: ignore existing probe cache and write a fresh result.
- `--skip-name TEXT`: skip files whose basename contains this text. Repeat the
  option for multiple markers.
- `--check-workers N`: set parallel eligibility check workers. Default: `4`.
- `--temp-dir PATH`: write probe sample files under this directory.
- `--keep-temp`: keep encoded probe samples.
- `--stall-timeout DURATION`: kill ffmpeg if frame progress stalls. Default:
  `10m`.

## Group Mode

Use `--group-crf` for related inputs where very different per-file sizes would
be distracting, such as multiple episodes from the same source:

```sh
reencode encode --group-crf *.mkv
```

Group mode probes every eligible file first. It starts from the most
conservative individual CRF, verifies that CRF across the group, and steps CRF
downward if needed until every file passes the quality checks.

If a shared CRF passes quality but exceeds the sample-size cap for some files,
`reencode` warns and keeps the shared CRF. Group mode prioritizes quality and
consistency over per-file size optimization.

Matching cache entries are reused, and extra CRF attempts tested while choosing
the shared CRF are written back to the probe cache.

## Progress, Cache, And Safety

Interactive terminals show compact stderr progress during sample encode, sample
VMAF scoring, and final encode. During probing, completed CRF attempts are
printed above the live progress line with VMAF, worst sample VMAF, encoded-size
percentage, and predicted output size. The selected CRF is printed before the
per-file summary.

Successful probe results are cached under:

```text
~/.cache/reencode/probe/
```

The cache key includes the current binary SHA-512, probe-affecting options,
cache schema version, and a fast input fingerprint. The input fingerprint uses
file size, mtime, media metadata, and SHA-512 over first/middle/last file
slices, so large videos do not need to be read in full for cache lookup.

Safety behavior:

- final outputs are validated before the source is removed
- unfinished final encodes and probe samples are cleaned up after failure or
  interruption
- `--keep-temp` preserves probe samples for inspection
- ffmpeg is killed when frame progress stalls longer than `--stall-timeout`
- stdout stays script-friendly, especially for `reencode probe --json`.

## Requirements

`ffmpeg` and `ffprobe` must be available in `PATH`.

The ffmpeg build must support:

- `libsvtav1`
- `libvmaf`

`reencode` checks these capabilities before probing or encoding and fails early
if a required encoder or filter is missing.

## Build

```sh
make build
```

The binary is written to:

```text
build/<goos>-<goarch>/bin/reencode
```

There is also a static build target:

```sh
make static
```
