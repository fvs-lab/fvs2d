# fvs2d (FUSE daemon)

The mount daemon for [FVS (Fused Versioned Storage)](https://github.com/fvs-lab/fvs2).
It exposes a committed state as a **read-only filesystem**, so you can browse and
read any past version live, without restoring it.

The mounted tree mirrors the committed state exactly (nested directories,
symlinks, empty files). Blocks are fetched on demand from the content-addressed
store and verified on read.

Several committed states can be stacked into a single view (lowest to highest),
where a higher layer overrides a path from a lower one and a `.wh.<name>`
whiteout marker removes it. An optional writable upper layer turns the mount
into a union filesystem: creates, writes, deletes and copy-ups land in a plain
directory that is itself ready to commit as a new state.

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

# stack several states low-to-high (-lower is repeatable: repo, repo@state, repo#branch)
fvs2d -mount /mnt/state -lower /path/to/base -lower /path/to/dotnet -lower /path/to/app

# add a writable upper layer (records all changes for capture as a new state)
fvs2d -mount /mnt/state -lower /path/to/base -lower /path/to/dotnet -upper /path/to/work

# check FUSE capabilities in the current environment
fvs2d -probe
```

Unmount with `fusermount3 -u /mnt/state` (or stop the daemon).

## Status and roadmap

- Read-only mounts of a single state or a stack of layers work today.
- A writable upper layer (`-upper`) works today: writes, creates, deletes and
  copy-ups are recorded in the upper directory; deletes are stored as
  `.wh.<name>` whiteout markers.
- Integrated `fvs mount` over IPC is planned; for now the daemon is driven
  directly via the flags above.

## License

MIT. See [LICENSE](LICENSE).
