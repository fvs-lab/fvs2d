# fvs2d (FUSE daemon)

The mount daemon for [FVS (Fused Versioned Storage)](https://github.com/fvs-lab/fvs2).
It exposes a committed state as a **read-only filesystem**, so you can browse and
read any past version live, without restoring it.

The mounted tree mirrors the committed state exactly (nested directories,
symlinks, empty files). Blocks are fetched on demand from the content-addressed
store and verified on read.

## Build

Default build (no FUSE backend, capability probe only):

```bash
go build -o ./bin/fvs2d ./cmd/fvs2d
```

FUSE3 build (requires `libfuse3-dev` and `pkg-config`):

```bash
CGO_ENABLED=1 go build -tags fuse3 -o ./bin/fvs2d ./cmd/fvs2d
```

## Usage

```bash
# mount the current HEAD of a repo (read-only)
fvs2d -repo /path/to/repo -mount /mnt/state

# or a specific branch / state id (prefix)
fvs2d -repo /path/to/repo -branch main  -mount /mnt/state
fvs2d -repo /path/to/repo -state 1f0247 -mount /mnt/state

# check FUSE capabilities in the current environment
fvs2d -probe
```

Unmount with `fusermount3 -u /mnt/state` (or stop the daemon).

## Status and roadmap

- Read-only mounts work today (the commands above).
- Writable mounts are not implemented yet (writes return `EROFS`).
- Integrated `fvs mount` over IPC is planned; for now the daemon is driven
  directly via the flags above.

## License

MIT. See [LICENSE](LICENSE).
