# Agent Notes

## Project Purpose

`reencode` is a command-line tool for unattended AV1 reencoding. It probes
source videos with ffmpeg and libvmaf, selects a CRF, encodes with `libsvtav1`
to `name_[e-av1].mkv`, validates the output, and only then removes the source.

Quality guardrails take priority over finding the smallest possible file. The
project shells out to `ffmpeg` and `ffprobe` rather than linking their libraries
or providing them. It is not a smallest-file-at-any-cost optimizer or an
embedded ffmpeg library.

## Repository Ownership

- `main.go` parses CLI arguments and dispatches subcommands. `help.go` owns
  user-facing command help.
- `eligibility.go` prefilters and classifies inputs while preserving input-order
  reporting. `ffprobe.go` owns content sniffing and ffprobe JSON parsing.
- `preflight.go` verifies external ffmpeg capabilities and fingerprints the
  external tools used by probing.
- `encode.go` owns final encoding, group mode, output validation, stream
  handling, logging, and source removal.
- `probe.go` owns sample encode/score work, CRF search, cache reuse, fallback,
  and outlier confirmation. `sample.go` owns sample-window selection.
- `timeline.go` owns decoded frame timestamps and alignment validation.
  `inspection.go` owns the shared ffprobe decoding-thread budget.
- `video_args.go` owns SVT-AV1 settings shared by probe and final encode.
  `vmaf.go` owns VMAF filter construction and score aggregation.
- `progress.go` owns ffmpeg process and progress parsing. `progress_display.go`
  owns terminal rendering.
- `cache.go` owns cache identity, persistence, and fast input fingerprinting.
- `util.go` owns output and temporary paths plus small filesystem helpers.
- `reencode_test.go` contains the test suite for the root `main` package.

## Commands

- `make test`: run `go test .` with repository-local Go state.
- `make test-integration`: build the actual CLI, then run opt-in media and
  fresh-process CLI regressions. Requires the runtime ffmpeg/ffprobe stack.
  CLI tracing requires a POSIX shell. The isolated CLI cache test uses Linux's
  `XDG_CACHE_HOME` override. These platform-specific tests skip elsewhere.
- `make -B build`: force a fresh binary at
  `build/<goos>-<goarch>/bin/reencode`.
- `make -B static`: force a stripped `CGO_ENABLED=0` binary at
  `build/<goos>-<goarch>/bin/reencode-static`.
- `make run ARGS='--help'`: build and run the local binary with arguments.
- `make GOTOOLCHAIN=local build`: require the installed Go toolchain instead of
  automatic toolchain selection.
- `make -n GOOS=<goos> GOARCH=<goarch> install`: inspect the install recipe
  without copying. Supply resolved platform values so Make does not run its
  platform-detection shell substitutions during the dry run.
- `make install`: install to `/usr/local/bin` when run as root, otherwise to
  `$HOME/.local/bin`.
- `make clean`: remove repository-local build state under `build/`.

Use `gofmt -w` on changed Go files. For documentation-only edits, inspect the
complete rendered text and run `git diff --check`.

## Behavior To Preserve

- Probe and final encode video settings must stay in sync. If SVT-AV1 args,
  keyframe interval, pixel format, or CRF handling change in one path, update
  the other path and tests.
- Final outputs are trusted only after `validateOutput` succeeds. Never remove
  a source before validation.
- Refuse an existing final output unless `--overwrite` is explicit. Even in
  overwrite mode, keep the existing output until its validated replacement is
  ready.
- Final encode temp files stay hidden and randomized and are cleaned on error,
  stall, or interruption. `--keep-temp` applies only to probe samples and must
  not preserve unfinished final encodes.
- Register probe sample temp paths before ffmpeg starts so interrupted sample
  encodes can be cleaned up.
- User-facing progress and status print only basenames through `displayPath`.
  Dry-run ffmpeg commands and log files may retain real paths.
- `probe --json` keeps stdout machine-readable. Progress and diagnostics belong
  on stderr or are disabled in JSON mode.
- ffmpeg progress uses `-progress pipe:1`, not parsed human stderr. Stall
  detection is based on frame progress because time and speed can move during
  an encoder hang.
- Interrupt errors intentionally omit ffmpeg stderr dumps to avoid noisy output
  after Ctrl-C.
- Input prefiltering uses standard-library content sniffing and accepts only
  `video/*`. ffprobe remains authoritative for candidates that pass sniffing.
- Final encode preserves source stream order, copies supported non-video
  streams, drops data streams, and converts FLAC audio to Opus unless
  `--no-audio-transcode` is set.
- Cache identity includes the executable hash, external-tool fingerprint,
  probe-affecting options, and a fast input fingerprint. UI, temp, and log
  options do not belong in the key.
- Fast input fingerprinting samples the first, middle, and last file slices.
  Preserve this bounded-I/O tradeoff unless a measured workflow justifies a
  change.
- Eligibility checks are bounded by the configured worker count while results
  and fatal-error selection remain in input order.
- Apply name skips before media inspection and already-AV1 encode skips before
  decoded-frame HDR inspection. Do not decode files that will be skipped.
  Forced reencodes and explicit probes must still validate actionable sources.
- Reuse eligibility metadata instead of repeating ffprobe/HDR inspection.
  Preserve its source snapshot and reject changed sources before reuse.
- Per-file input checks defer only decoded HDR-prefix inspection. Metadata
  safety and path conflicts remain upfront. Require explicit validated state
  before CRF overrides, cache lookup, fallback, or encoding. Do not prefetch
  decoded checks or permit per-file scheduling with group CRF. Release large
  per-window state after each file's last use in ordinary batches.
- Frame inspection shares `max(1, GOMAXPROCS/4)` decoder threads across workers.
  Do not give each worker the entire budget or use automatic/all-core decoding.
  This limits inspection decoder threads, not final encoding or VMAF scoring.
- Group mode starts from the most conservative individual CRF and only moves
  downward. It favors shared quality consistency over per-file size optimality.
- Persist successful group and single-file probe attempts before applying
  `--fallback-crf` so retries can reuse completed work.
- Persist complete, aligned window measurements before quality decisions and
  only after rechecking the source snapshot. Measurement identity includes
  actual encoder args and timing/scoring policy, not decision thresholds.
  Recompute decisions on reuse, never synthesize passes from partial windows.
  `--no-cache` means no persistent I/O and refresh ignores preexisting records.
- VMAF scoring resets timestamps on both streams and uses harmonic mean. The
  weighted mean must meet the target, while the worst sample must meet the
  separate floor unless nearby-window confirmation accepts one borderline
  outlier.
- CRF search uses quarter-step quantization. Prefer `qFromCRF` and `crfFromQ`
  over direct floating-point comparisons.
- Window priority may use the nearest measured CRF, with lower-CRF and original
  index tie breaks. Keep aggregation and outlier indices in canonical order.
  Partial attempts prove floor failure only, never a mean/size-based search stop.

## Operational Constraints

- The module targets Go 1.26. Make defaults to `GOTOOLCHAIN=auto` and keeps Go
  caches, module state, GOPATH, temporary files, and telemetry under `build/`.
- Runtime probing and encoding require `ffmpeg` and `ffprobe` in `PATH`, with
  `libsvtav1` and `libvmaf` available as required by the selected command.
- Probe results are persistent user state under `~/.cache/reencode/probe/`.
  Changes to cache schema or identity require compatibility and reuse tests.
- The Go module currently uses only the standard library. Do not add a module
  dependency without a concrete need and user agreement.
- There is no separate CI, release, generation, container, or frontend build
  configuration. Preserve the Makefile as the build workflow source of truth.

## Testing Focus

Add focused tests when changing:

- CLI parsing, help text, defaults, exit codes, or stdout/stderr separation;
- content sniffing, ffprobe parsing, eligibility concurrency, or input ordering;
- CRF search direction, quarter-step boundaries, fallback, and cache reuse;
- cache keys, external-tool identity, fingerprints, and persisted attempts;
- probe/final ffmpeg arguments, SVT-AV1 parity, and VMAF filter construction;
- output validation, temp cleanup, source removal, Ctrl-C, and stall behavior;
- progress parsing, basename-only display, and terminal formatting;
- group CRF selection, outlier confirmation, and size-versus-quality rules.

## Performance Guardrails

- `ffprobe -show_frames` decodes video. It is not a cheap metadata query, even
  if `-show_entries` requests only timestamps. Configure decoder threads
  explicitly and account for worker concurrency as well as per-process cost.
- Before adding an extra decode pass, measure representative compressed media
  and large batches, including already-encoded inputs. Tiny synthetic fixtures
  establish correctness but can hide substantial decoding costs.
- Preserve bounded sample windows and input-order results. Seeking may land on
  an earlier keyframe, so filter decoded timestamps to the requested interval.
- Alignment checks protect VMAF frame pairing, particularly for VFR input.
  Optimize redundant decoding without substituting nominal FPS or packet
  counts for decoded-frame correctness. Collecting timestamps during existing
  decoding is preferable when equivalence has been established and tested.
- Decoded timestamp capture uses wrapped AVFrame references and framecrc, not
  raw-video serialization. Observe both streams before timestamp normalization.
  Test unequal stream lengths so shortest-frame synchronization cannot hide
  extra frames. Sidecars are scratch state even with `--keep-temp`.
- Select the capture backend by a runtime capability check and include its
  versioned timing policy in cache identity. Only an explicit missing capture
  feature on ordinary process exit permits compatibility inspection. Setup
  errors, including filesystem faults, malformed captures, coverage mismatch,
  and timeouts, stop the command without backend or CRF fallback. Do not cache
  failed checks. Parent cancellation remains exit 130. Check capture before
  model setup and before final encoding with quality verification.
  Malformed captures and alignment errors during scoring are fatal.
  Source-window facts may be shared while their snapshot matches, but final
  quality verification must always score the actual final output.
- Cover skip-before-decode ordering, metadata reuse, shared thread limits,
  cancellation while waiting for a decoder slot, and threaded timestamp parity.
- Backend changes require fresh-process CLI coverage with default temp settings,
  not only direct-function tests or forced backend selection. Assert the timing
  policy and subprocess arguments: capture-mode probing must not run per-window
  ffprobe timestamp scans. Keep the bounded HDR-prefix check. Exercise explicit
  unsupported features separately from operational faults and interruption.
- Long inspection and batch-check phases need visible status. Distinguish
  elapsed-time activity from measured progress and stall detection. File-level
  activity scopes survive subprocess handoffs. Only the visible scope may
  redraw, and each scope must stop and join its ticker on return, including on
  error or Ctrl-C. Route diagnostics inside scopes through the progress display
  so background redraws cannot erase permanent output. Keep JSON and
  `--no-progress` runs free of interactive output.
- Timing reports describe the current invocation, never historical cached work.
  Count shared preparation once in comparisons and exclude eligibility queue
  waits from per-file active time. Command wall time includes all batch waiting.

Before finishing non-trivial changes, run:

```sh
make test
make -B build
```

Run `make -B static` when build flags, platform handling, or process boundaries
change.

## Comments And Documentation

Use maintenance comments to explain rationale, coupling, cleanup ownership,
ordering, or external-tool quirks that are not obvious from the code. The main
hazards are shared probe/final video settings, validation-before-removal,
temporary-path registration and cleanup, cache identity, ordered concurrent
eligibility, frame-based stall detection, VMAF timestamp/model choices, and
group fallback persistence.

Do not restate the next line or invent history. If repository evidence does not
establish a rationale, report the uncertainty instead of encoding a guess in a
comment.

Keep `AGENTS.md` agent-facing and the README user-facing. The README presents
the command-line tool rather than the Go project, uses GNU-style long options,
avoids semicolons in prose, and keeps build and install guidance near the end.
