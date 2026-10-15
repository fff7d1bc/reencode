#!/bin/sh
# Test-only PATH shim. All non-injected work uses the real installed media tools.
set -eu
tool=${0##*/}
trace=$(mktemp "$REENCODE_CLI_TRACE/$tool.XXXXXX")
printf '%s\n' "$tool" "$@" > "$trace"
capture=
for arg do
    case "$arg" in
        */.reencode-capture-check-*.crc) capture=$arg ;;
    esac
done
if [ -n "$capture" ]; then
    case "$REENCODE_CLI_CAPTURE_FAULT" in
        encoder) printf "Unknown encoder 'wrapped_avframe'\n" >&2; exit 1 ;;
        muxer) printf "Requested output format 'framecrc' is not known.\n" >&2; exit 1 ;;
        timebase) printf 'Invalid time base: filter\n' >&2; exit 1 ;;
        unknown) printf 'Invalid argument\n' >&2; exit 1 ;;
        missing) exit 0 ;;
        malformed) printf 'invalid capture\n' > "$capture"; exit 0 ;;
        coverage)
            printf '%s\n' '#tb 0: 1/24' '#media_type 0: video' '#codec_id 0: wrapped_avframe' \
                '#tb 1: 1/24' '#media_type 1: video' '#codec_id 1: wrapped_avframe' \
                '0, 0, 0, 1, 424, ignored' '1, 0, 0, 1, 424, ignored' > "$capture"
            exit 0 ;;
        signal) kill -KILL "$$" ;;
        wait)
            printf 'partial capture\n' > "$capture"
            printf 'ready\n' > "$REENCODE_CLI_READY"
            exec sleep 60 ;;
    esac
fi
case "$tool" in
    ffmpeg) exec "$REENCODE_CLI_REAL_FFMPEG" "$@" ;;
    ffprobe) exec "$REENCODE_CLI_REAL_FFPROBE" "$@" ;;
    *) exit 99 ;;
esac
