# Bundled VMAF models

Unmodified Netflix VMAF v1.0.16 SDR models from release v3.2.1, commit
`f85a853692a8c730d0270cd733c8bb30b5b93b7c`:

https://github.com/Netflix/vmaf/tree/f85a853692a8c730d0270cd733c8bb30b5b93b7c/model

The `vmaf_v1.0.16` and `vmaf_v1.0.16_hfr` directories provide the standard
1080p 3H and 4K 1.5H variants. Their original filenames are preserved here.
The accompanying upstream LICENSE covers these assets.

Models are embedded in the executable. Changing these files changes their
SHA-512 identity and invalidates affected probe caches. Runtime downloads and
automatic model discovery are intentionally not used.
