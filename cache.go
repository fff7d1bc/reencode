package main

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const probeCacheSchemaVersion = 1
const probeCacheSampleSize = 1024 * 1024

type probeCacheOptions struct {
	Preset            string  `json:"preset"`
	TargetVMAF        float64 `json:"target_vmaf"`
	FloorVMAF         float64 `json:"floor_vmaf"`
	MaxEncodedPercent float64 `json:"max_encoded_percent"`
	NoOutlierCheck    bool    `json:"no_outlier_check"`
	Samples           int     `json:"samples"`
	SampleDurationNS  int64   `json:"sample_duration_ns"`
}

type probeCacheHandle struct {
	Root             string
	Path             string
	BinaryHash       string
	InputFingerprint inputFingerprint
	FingerprintKey   string
	Options          probeCacheOptions
	OptionsKey       string
	SourcePath       string
}

type probeCacheEnvelope struct {
	SchemaVersion    int               `json:"schema_version"`
	BinaryHash       string            `json:"binary_hash"`
	InputFingerprint inputFingerprint  `json:"input_fingerprint"`
	Options          probeCacheOptions `json:"options"`
	SourcePath       string            `json:"source_path"`
	CreatedAt        time.Time         `json:"created_at"`
	Result           ProbeResult       `json:"result"`
}

type inputFingerprint struct {
	Size          int64   `json:"size"`
	ModeRegular   bool    `json:"mode_regular"`
	ModTimeUnixNS int64   `json:"mtime_unix_ns"`
	DurationNS    int64   `json:"duration_ns"`
	FPS           float64 `json:"fps"`
	TotalFrames   int64   `json:"total_frames"`
	VideoBytes    int64   `json:"video_bytes"`
	VideoCodec    string  `json:"video_codec"`
	PixelFormat   string  `json:"pixel_format"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	SampleHash    string  `json:"sample_hash"`
}

func prepareProbeCache(opts ProbeOptions, file string) (*probeCacheHandle, error) {
	if opts.NoCache {
		return nil, nil
	}
	root, err := defaultProbeCacheRoot()
	if err != nil {
		return nil, err
	}
	binaryHash, err := currentExecutableHash()
	if err != nil {
		return nil, err
	}
	// Cache identity includes the current executable. Probe behavior is still
	// evolving, so a rebuilt binary must not silently trust older measurements.
	info, err := probeMedia(file)
	if err != nil {
		return nil, err
	}
	fingerprint, err := fastInputFingerprint(file, info)
	if err != nil {
		return nil, err
	}
	fingerprintKey, err := inputFingerprintKey(fingerprint)
	if err != nil {
		return nil, err
	}
	options := normalizedProbeCacheOptions(opts)
	optionsKey, err := probeCacheOptionsKey(options)
	if err != nil {
		return nil, err
	}
	return newProbeCacheHandle(root, binaryHash, fingerprint, fingerprintKey, options, optionsKey, file), nil
}

func defaultProbeCacheRoot() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "reencode", "probe"), nil
}

func newProbeCacheHandle(root, binaryHash string, fingerprint inputFingerprint, fingerprintKey string, options probeCacheOptions, optionsKey, sourcePath string) *probeCacheHandle {
	return &probeCacheHandle{
		Root:             root,
		Path:             filepath.Join(root, binaryHash, fingerprintKey, optionsKey+".json"),
		BinaryHash:       binaryHash,
		InputFingerprint: fingerprint,
		FingerprintKey:   fingerprintKey,
		Options:          options,
		OptionsKey:       optionsKey,
		SourcePath:       sourcePath,
	}
}

func normalizedProbeCacheOptions(opts ProbeOptions) probeCacheOptions {
	// Only include options that affect probe results. UI/logging/temp settings
	// must not split cache entries for identical measurements.
	return probeCacheOptions{
		Preset:            opts.Preset,
		TargetVMAF:        opts.TargetVMAF,
		FloorVMAF:         opts.FloorVMAF,
		MaxEncodedPercent: opts.MaxEncodedPercent,
		NoOutlierCheck:    opts.NoOutlierCheck,
		Samples:           opts.Samples,
		SampleDurationNS:  int64(opts.SampleDuration),
	}
}

func probeCacheOptionsKey(options probeCacheOptions) (string, error) {
	data, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:]), nil
}

func loadProbeCache(handle *probeCacheHandle, currentPath string) (ProbeResult, bool, error) {
	data, err := os.ReadFile(handle.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return ProbeResult{}, false, nil
		}
		return ProbeResult{}, false, err
	}
	var envelope probeCacheEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return ProbeResult{}, false, err
	}
	if !probeCacheEnvelopeMatches(envelope, handle) {
		return ProbeResult{}, false, nil
	}
	if !envelope.Result.Success {
		return ProbeResult{}, false, nil
	}
	result := envelope.Result
	// The cache is content-addressed enough to survive renames. Return the path
	// the user passed today so progress, summaries, and JSON refer to it.
	result.File = currentPath
	return result, true, nil
}

func storeProbeCache(handle *probeCacheHandle, result ProbeResult) error {
	dir := filepath.Dir(handle.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	envelope := probeCacheEnvelope{
		SchemaVersion:    probeCacheSchemaVersion,
		BinaryHash:       handle.BinaryHash,
		InputFingerprint: handle.InputFingerprint,
		Options:          handle.Options,
		SourcePath:       handle.SourcePath,
		CreatedAt:        time.Now().UTC(),
		Result:           result,
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-probe-cache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	// Write through a temp file so an interrupted cache update never leaves a
	// truncated JSON document that future runs might try to parse.
	if err := os.Rename(tmpName, handle.Path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func probeCacheEnvelopeMatches(envelope probeCacheEnvelope, handle *probeCacheHandle) bool {
	return envelope.SchemaVersion == probeCacheSchemaVersion &&
		envelope.BinaryHash == handle.BinaryHash &&
		envelope.InputFingerprint == handle.InputFingerprint &&
		envelope.Options == handle.Options
}

func currentExecutableHash() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return fileSHA512(path)
}

func fileSHA512(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fastInputFingerprint(path string, info MediaInfo) (inputFingerprint, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return inputFingerprint{}, err
	}
	hash, err := sampledFileSHA512(path, stat.Size(), probeCacheSampleSize)
	if err != nil {
		return inputFingerprint{}, err
	}
	return inputFingerprint{
		Size:          stat.Size(),
		ModeRegular:   stat.Mode().IsRegular(),
		ModTimeUnixNS: stat.ModTime().UnixNano(),
		DurationNS:    int64(info.Duration),
		FPS:           info.FPS,
		TotalFrames:   info.TotalFrames,
		VideoBytes:    info.VideoBytes,
		VideoCodec:    info.VideoCodec,
		PixelFormat:   info.PixelFormat,
		Width:         info.Width,
		Height:        info.Height,
		SampleHash:    hash,
	}, nil
}

func inputFingerprintKey(fingerprint inputFingerprint) (string, error) {
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return "", err
	}
	sum := sha512.Sum512(data)
	return hex.EncodeToString(sum[:]), nil
}

func sampledFileSHA512(path string, size int64, sampleSize int64) (string, error) {
	if sampleSize <= 0 {
		sampleSize = probeCacheSampleSize
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if size <= sampleSize*3 {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	// Hashing entire multi-GB videos made cache lookup too expensive. Sampling
	// the beginning, middle, and end is a fast fingerprint, not a cryptographic
	// proof of file identity.
	offsets := []int64{0, (size - sampleSize) / 2, size - sampleSize}
	buf := make([]byte, 128*1024)
	for _, offset := range offsets {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyBuffer(h, io.LimitReader(f, sampleSize), buf); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func warnCache(opts ProbeOptions, format string, args ...any) {
	if !opts.Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
