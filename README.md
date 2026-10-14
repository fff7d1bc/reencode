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

Use `--keep-source` to retain the original after a successful encode. This also
works with explicit CRF, fallback CRF, and group mode. Output validation and
overwrite protections still apply.

Final ffmpeg decoding and filtering errors are fatal. Before publishing,
`reencode` verifies AV1 video, display-oriented dimensions, mapped stream layout,
and duration and estimated frame counts within the greater of one percent or
two seconds. Validation failure keeps the source and removes the unfinished
output.
When frame estimates disagree, both files are decoded to count frames before
deciding. This extra check handles variable-frame-rate files whose reported
frame rate changes between containers, but can make validation take longer.
Display-matrix rotation is applied by ffmpeg during sample and final encoding.
Probing and validation use the resulting orientation, including swapped width
and height for portrait videos carrying a 90° or 270° rotation.

For an additional quality gate, use `encode --verify-quality`. After structural
validation it scores the actual temporary final output against the original,
using the planned sample windows, selected VMAF profile, effective target, and
sample floor. With explicit or fallback CRF, it uses `--target-vmaf`. A borderline
outlier must be confirmed again on neighboring windows of the final output.
This is sampled assurance, not a full-video quality guarantee. Small files whose
probe plan is full-pass are scored to EOF.

Failed quality, incomplete scores, timestamp misalignment, or interruption
prevents publication and source removal. An existing output is preserved and
the failed final temp is removed even with `--keep-temp`. No automatic lower-CRF
retry is attempted. Verification is off by default and requires libvmaf even
with `--crf`. `--dry-run` does not run final verification.

Final encode keeps the mapped source order for video, audio, subtitle, and
attachment streams. The primary video becomes AV1. FLAC audio streams are
converted to Opus at `256000` bps while other mapped codecs are copied. Data and
unknown stream types are dropped with a warning. Use `--no-audio-transcode` to
copy all mapped audio streams unchanged.

Final outputs include `REENCODE_*` MKV metadata with the selected codec,
encoder, preset, CRF, pixel format, and successful probe VMAF scores when a
probe result selected the CRF.

Outputs are written next to the input:

```text
name_[e-av1].mkv
```

The final commit is serialized per output and rechecks the destination before
publishing. Concurrent commands targeting the same output fail safely instead
of replacing each other. If a process is killed during this brief commit phase,
it can leave a hidden `*.reencode-lock` directory. Remove that directory only
after confirming that no matching `reencode` process is active.

Inputs are skipped when built-in content sniffing does not identify them as
video files. Common QuickTime MOV headers are recognized regardless of filename
extension. ffprobe must still confirm a valid video stream.

Actionable inputs whose primary video carries alpha (transparency) are rejected
before probing or encoding. The AV1 output cannot preserve alpha, and VMAF cannot
detect its loss. This safety check also applies with `--crf`, `--fallback-crf`,
and `--force-video`. The source remains unchanged.

Detected HDR inputs are also rejected, including PQ/HDR10, HLG, Dolby Vision,
and HDR10+. The tool checks primary-stream metadata and a bounded prefix of
decoded frames. VMAF's public models are SDR models, so neither `--crf` nor
`--fallback-crf` bypasses this safety check. Missing or incorrect source metadata
can prevent HDR detection. Ten-bit video and BT.2020 primaries alone are not
classified as HDR.

Inputs are also skipped when they are already `.mkv` files whose primary video
codec is AV1. Use `--force-reencode` to bypass the already-AV1 skip. Use
`--skip-name TEXT` to skip inputs whose basename contains a marker you choose,
such as `[reencoded]`. The option can be repeated.

Name filters run before media inspection. Already-AV1 skips use basic stream
metadata and do not decode frames for HDR or timestamp checks. Files selected
for processing still undergo the HDR and alpha checks, including forced
reencodes. Inspected metadata is reused while the source identity, size, and
modification time remain unchanged.

Some valid containers, including MPEG transport streams, are not recognized by
the built-in sniffer. Use `--force-video` to send regular-file inputs directly
to ffprobe. The flag bypasses only content sniffing. ffprobe must still find a
valid video stream and usable media metadata.

When more than one input is passed, `reencode` checks eligibility first and
prints a compact summary before starting probe or encode work. The work counter
then counts only actionable files, not every path matched by the shell. This
check runs up to four files in parallel by default. Use `--check-workers N` to
adjust it.

Frame inspection has a shared decoder-thread budget of one quarter of the
available logical CPUs, rounded down with a minimum of one. CPU availability
follows Go's `GOMAXPROCS`, including configured limits. Concurrent eligibility
checks divide that budget, with excess decoding work waiting for a slot.
For example, 32 available CPUs allow eight threads for a sequential timestamp
scan or two threads each for four simultaneous HDR checks. This is a decoder
thread budget, not a hard CPU-usage limit. Final encoding and VMAF scoring keep
their own threading behavior.

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
   Windows are bounded by playback time, not average-FPS frame estimates.
   Full-pass probes run to EOF. Decoded frame timestamps are checked before
   accepting scores, and measured window coverage weights scores and size estimates.
3. Encode each sample window with the same SVT-AV1 settings used for the final
   encode, including preset, CRF, pixel format, keyframe interval, and SVT
   parameters.
4. Score each encoded sample against the matching source window with ffmpeg
   `libvmaf`.
5. Search CRF values and select the highest CRF that still satisfies the
   quality rules and sample-size cap.

On supported ffmpeg builds, timestamp validation observes the frames already
being decoded for VMAF. It does not run separate source and sample timestamp
decode passes. A small startup capability check verifies this backend,
including unequal stream lengths. Older builds fall back to bounded ffprobe
inspection with a diagnostic. Malformed captures and alignment failures are
errors, not reasons to retry with weaker validation.

Bounded windows decode up to two extra seconds of source context to measure
the last frame's coverage. That context is neither encoded nor scored. A
two-hour input normally uses twelve 20-second windows, not a full-video scan.
`--verify-quality` always scores the actual final output. It reuses source
window facts when available, otherwise it inspects those bounded source windows
to make precise cuts. No probe score substitutes for final-output verification.

The CRF search starts with a broad binary-search style probe, then uses already
measured pass/fail points to estimate the next useful CRF. This keeps the search
conservative while avoiding obvious wasted probes when the quality boundary is
already clear. If a CRF has already become impossible because sample scores are
too far below the VMAF floor, `reencode` stops that CRF early and moves on
instead of scoring the remaining sample windows.

The VMAF scoring path is intentionally conservative:

- timestamps are normalized before scoring
- source and encoded samples use a shared pixel format
- VMAF uses `pool=harmonic_mean`
- scoring requires a complete libvmaf JSON report, not just a successful process
- VMAF model selection is explicit and deterministic
- the default `v1` profile uses bundled, pinned VMAF v1.0.16 SDR models
- normal sources use the standard 1080p 3H model and 4K-oriented sources use
  the 4K 1.5H model, with portrait orientation handled equivalently
- sources averaging at least 45 fps use the corresponding HFR variant
- CAMBI uses actual encoded dimensions and 10-bit depth before scoring upscale
- low-resolution sources may be scaled for VMAF only
- the final decision uses both a weighted mean score and a worst-sample floor.

Probe JSON includes per-sample diagnostics. `--verbose` also prints them on
stderr: sample coverage, harmonic score, the fifth percentile of frame scores,
and the worst rolling one-second harmonic score and its source timestamp.
For samples shorter than a second, the rolling diagnostic covers the available
duration. It uses actual frame durations for VFR video. These diagnostics do
not add quality gates. Raw scoring reports are temporary and are not retained
by `--keep-temp`.

`reencode` does not try to discover and use the newest installed VMAF model at
runtime. Model choice changes the score, so automatic discovery would make
results vary across machines and package upgrades. The selected model is part
of the probe cache identity.

Use `--vmaf-profile legacy` to retain the previous v0.6.1 model and scaling
policy. V1 requires at least one encoded dimension of 216 pixels. A synthetic
startup check loads the selected model, exercises its features, and validates
complete scores. Compatibility is determined by this check, not a numeric
version gate. Unsupported runtimes fail explicitly, without silently switching
models.

Libvmaf 3.2.1 or newer is recommended for its
[thread-queue backpressure fix](https://github.com/Netflix/vmaf/releases/tag/v3.2.1),
which avoids excessive memory growth during threaded scoring. The short startup
check cannot detect that issue. Some 3.2.1 builds report `3.2.0` or a commit hash
because of [upstream version metadata](https://github.com/Netflix/vmaf/blob/v3.2.1/libvmaf/meson.build).
JSON and cache identity retain the reported libvmaf and SVT-AV1 version strings
unchanged. JSON also records the model ID, SHA-512, and scoring policy.
Successful probe metadata identifies the model.
Old cache entries remain on disk but are not reused by the new schema.
Bundled model provenance and licensing are in `models/`. A standalone binary
also exposes the upstream license through `reencode help licenses`.

## Target VMAF And Floor

`--target-vmaf` and `--vmaf-floor` are separate checks.

The reported `avg vmaf` is the duration-weighted mean across all probe samples.
It must meet `--target-vmaf`, which defaults to `95`.

The reported `min vmaf` is the lowest individual sample score. It must meet
`--vmaf-floor`, which defaults to `94`.

The floor is not a fallback average target. If the weighted mean does not meet
`--target-vmaf`, probing fails even when every sample meets the floor.

Examples:

- `avg vmaf 95.2, min vmaf 94.4`: pass
- `avg vmaf 95.2, min vmaf 91.0`: fail, at least one section is too damaged
- `avg vmaf 94.8, min vmaf 94.5`: fail, samples are consistent but the average
  quality is below target.

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

`encode` accepts the shared probing options below because it normally probes
before doing the final encode.

### `probe`

```sh
reencode probe [options] FILE...
```

Probe files and report the selected CRF without final encoding. With `--json`,
results are written as JSONL on stdout.

`probe` accepts the same shared probing options as `encode`, plus `--json` for
machine-readable output.

### `compare`

```sh
reencode compare --no-cache file.mkv
reencode compare --tunes psnr,vq,vmaf --json file.mkv
```

`compare` defaults to `--vmaf-profiles legacy,v1` and `--tunes psnr`. It runs
independent CRF searches on the same inspected source and sample-window policy.
The table reports CRF, effective target, quality diagnostics, predicted size,
and total sample-encoding and scoring time across each search. JSONL writes
one object per input in input order, with individual results for every
profile/tune combination. Failures are reported without hiding other rows.

Cache hits are labelled and historical timings are cleared. Current-run cache
lookup and runtime-check costs remain visible. Shared metadata and HDR
preparation are reported once, separately from the individual searches.
Use `--no-cache` to
measure fresh searches. These are search costs, not predicted final-encode
times, and different searches may evaluate different numbers of CRFs. VMAF
scores from different model families are not interchangeable. Compare never
publishes final outputs or removes sources. Other shared probe options apply.

## Options

### Shared By `encode` And `probe`

These options control input checks, sample selection, CRF search, VMAF scoring,
cache behavior, temporary probe files, and progress. They apply to `encode`
because final encoding uses the same probe engine unless `--crf` bypasses
probing.

- `--preset N`: use a different SVT-AV1 preset. Default: `4`.
- `--tune psnr|vq|vmaf`: encoder tuning, explicitly `psnr` by default. The
  mapping is SVT tune 1, 0, and 5 respectively. A small startup encode checks
  support before processing inputs. Probe and final settings always match,
  and tune is recorded in JSON, MKV metadata, and the cache key. VMAF tuning
  optimizes the encoder objective, it does not select the scoring model or
  replace visual inspection.
- `--vmaf-profile v1|legacy`: select the scoring model family. Default: `v1`.
- `--target-vmaf N`: weighted mean VMAF target. Default: `95`.
- `--vmaf-floor N`: worst-sample floor. Default: `94`.
- `--max-encoded-percent N`: maximum encoded sample size percent. Default: `90`.
- `--samples N`: override automatic sample count.
- `--sample-duration DURATION`: duration of each sample. Default: `20s`.
- `--no-outlier-check`: disable nearby-window confirmation for one borderline
  low sample.
- `--no-cache`: bypass probe cache.
- `--refresh-cache`: ignore existing probe cache and write a fresh result.
- `--force-video`: bypass content sniffing for regular files and require
  ffprobe to find video.
- `--skip-name TEXT`: skip files whose basename contains this text. Repeat the
  option for multiple markers.
- `--check-workers N`: set parallel eligibility check workers. Default: `4`.
- `--temp-dir PATH`: write probe sample files under this directory.
- `--keep-temp`: keep encoded probe samples.
- `--stall-timeout DURATION`: kill ffmpeg if frame progress stalls. Zero
  disables stall detection. Default: `10m`.
- `--no-progress`: disable the interactive progress display.
- `--verbose`: print extra details.

### `encode` Only

- `--crf N`: bypass probing and encode with this CRF.
- `--fallback-crf N`: use this CRF when probing fails.
- `--group-crf`: probe all inputs first and encode with one shared CRF.
- `--overwrite`: allow replacing the unchanged output file that existed during
  planning, including an output included by a wildcard but skipped during
  eligibility. It does not allow conflicts with inputs that will be encoded or
  replacing a destination that appeared after encoding began.
- `--force-reencode`: encode even when the input is already `.mkv` with AV1
  video.
- `--no-audio-transcode`: copy all mapped audio streams without converting
  FLAC to Opus.
- `--dry-run`: print the final ffmpeg command without encoding.
- `--keep-source`: retain the original after successful validation and publication.
- `--verify-quality`: verify sampled quality of the actual final encode before
  publication. Default: off.
- `--log-file PATH`: append before/after size records.

### `probe` Only

- `--json`: write JSONL results to stdout.

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

Group encoding is intentionally non-transactional. Each successfully validated
file is published and its source is removed (unless `--keep-source` is set)
before the next file begins. If a
later encode fails, earlier successes remain committed and are not rolled back.
A file that fails before its commit keeps its source. All unprocessed sources
remain untouched.

## Progress, Cache, And Safety

Interactive terminals show checked/skipped counts during input classification,
elapsed-time status during source and encoded-sample timestamp inspection, and
progress during runtime checks, sample encode, VMAF scoring, and final encode.
Inspection elapsed time is not a percentage or a stall-detection heartbeat.
These displays are disabled by `--no-progress` and in JSON mode.
Fresh searches announce their sample count, sampled duration, and inspection
thread budget. JSON includes current-run preparation, timestamp inspection,
runtime checks, cache, encode, score, and active-total timings. `--verbose`
prints these timings and separate whole-command wall time. Active time excludes
waiting behind other files and decoded-inspection slots, not subprocess work.
During probing, completed CRF attempts are
printed above the live progress line with average VMAF, minimum sample VMAF,
encoded-size percentage, and predicted output size. The selected CRF is printed
before the per-file summary.

Successful probe results are cached under:

```text
~/.cache/reencode/probe/
```

Each fully scored, timestamp-validated window is also saved immediately. An
interrupted or unsuccessful search can resume those measurements, including
scores below the current target. Records contain scores, frame timing, and byte
counts, not encoded video. Changing the target, floor, size cap, or outlier
policy reuses compatible measurements but recomputes the decision. JSON's
`reused_measurements` and human summaries distinguish this from a complete
`cache_hit`. Incomplete windows are never treated as passing measurements.

`--no-cache` disables persistent reads and writes. `--refresh-cache` ignores old
disk records while allowing reuse of work completed within this invocation.
Corrupt window records are reported and measured again. Cache write errors do
not discard valid probe work. Older schema files remain on disk and are not
reused. Final quality verification never reuses probe quality measurements.

The cache key includes the current binary SHA-512, probe-affecting options,
cache schema version, an external ffmpeg/ffprobe fingerprint, and a fast input
fingerprint. The external fingerprint is based on ffmpeg and ffprobe version
output plus the SVT-AV1 encoder and libvmaf filter help output. Actual library
versions, bundled model bytes, and scoring configuration also enter the key.
The input fingerprint uses file size, mtime, media metadata, and SHA-512 over
first/middle/last file slices, so large videos do not need to be read in full
for cache lookup.

Safety behavior:

- final outputs are validated before the source is removed
- source identity, size, and mtime are rechecked before publication, and the
  exact source entry is quarantined and verified before removal
- duplicate destinations, output/input overlap, duplicate physical inputs, and
  log-file aliases fail before probing or encoding
- unfinished final encodes and probe samples are cleaned up after failure or
  interruption
- `--keep-temp` preserves probe samples for inspection
- ffmpeg is killed when frame progress stalls longer than `--stall-timeout`
- stdout stays script-friendly, especially for `reencode probe --json`.

## Requirements

`ffmpeg` and `ffprobe` must be available in `PATH`.

The ffmpeg build must support:

- `libsvtav1`
- `libvmaf` with support for the selected model's features, with 3.2.1 or newer
  recommended for the default v1 profile

`reencode` checks these capabilities before probing or encoding and fails early
if a required encoder or filter is missing.
Explicit `--crf` encoding without quality verification does not require libvmaf.

## FFmpeg Builds

Use a recent full-featured ffmpeg build when possible. Probe scores, encoder
behavior, and available filters can vary across ffmpeg releases and distro
package builds.

On Linux, one practical option is Homebrew for Linux, also known as linuxbrew.
Its `ffmpeg-full` package is often a convenient way to get an ffmpeg build with
SVT-AV1 and libvmaf support. The regular Homebrew `ffmpeg` package is not the
right package for this tool when it does not expose `libsvtav1`.

```sh
brew install ffmpeg-full
```

`ffmpeg-full` is keg-only. That means Homebrew installs it but does not link it
as the default `ffmpeg` in the normal Homebrew prefix. After installing, make
sure the `ffmpeg` and `ffprobe` found first in `PATH` are the full Homebrew
versions, not a stripped-down system package or the regular Homebrew `ffmpeg`:

```sh
which -a ffmpeg
which -a ffprobe
ffmpeg -hide_banner -encoders | grep libsvtav1
ffmpeg -hide_banner -filters | grep libvmaf
```

`which -a ffmpeg` should show the `ffmpeg-full` binary before any other ffmpeg.
If it does not, follow the `brew info ffmpeg-full` PATH instructions for your
installation.

## Build

The module targets Go 1.26. Make keeps Go caches, downloaded module and
toolchain state, temporary files, and telemetry under `build/`.

Automatic Go toolchain selection remains enabled by default. To require the
installed toolchain instead, pass `GOTOOLCHAIN=local`:

```sh
make GOTOOLCHAIN=local build
```

Run the test suite and build the local binary with:

```sh
make test
make build
```

To also run real-media regression tests, install the runtime ffmpeg and ffprobe
requirements and run:

```sh
REENCODE_INTEGRATION=1 make test
```

The binary is written to:

```text
build/<goos>-<goarch>/bin/reencode
```

Windows builds use the `.exe` suffix. A Windows amd64 build can be produced
from any supported Go host with:

```sh
make GOOS=windows GOARCH=amd64 build
```

The result is `build/windows-amd64/bin/reencode.exe`. At runtime, Windows also
requires `ffmpeg.exe` and `ffprobe.exe` to be available in `PATH`.

There is also a static build target:

```sh
make static
```

Use `make -B build` or `make -B static` to force a fresh build. For local
development, build and run the command with arguments through `ARGS`:

```sh
make run ARGS='--help'
```

## Install

The Makefile install target is for Unix systems. Windows users should place the
built `.exe` in a directory listed in `PATH`.

```sh
make install
```

When run as root, this installs `reencode` to `/usr/local/bin`. Otherwise it
installs to `$HOME/.local/bin`.

Repository-local build state can be removed with:

```sh
make clean
```
