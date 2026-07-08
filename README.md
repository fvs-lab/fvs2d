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

The daemon uses the pure-Go
[go-fuse](https://github.com/hanwen/go-fuse) client. Only the runtime `fusermount3` helper and `/dev/fuse` are needed.

```bash
go build -o ./bin/fvs2d ./cmd/fvs2d
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

# enable the gRPC control API (status and clean shutdown over a socket or TCP)
fvs2d -repo /path/to/repo -mount /mnt/state -control unix:/run/user/1000/fvs2d.sock
```

Unmount with `fusermount3 -u /mnt/state`, by stopping the daemon, or through the
control API (`Shutdown`).

## Status and roadmap

- Read-only mounts of a single state or a stack of layers work today.
- A writable upper layer (`-upper`) works today: writes, creates, deletes and
  copy-ups are recorded in the upper directory; deletes are stored as
  `.wh.<name>` whiteout markers.
- The gRPC control API (`-control`) exposes `Health`, `GetStatus` and
  `Shutdown`, so a supervisor can introspect the mount and stop it cleanly
  without signals. The `fvs2` CLI drives the daemon over it (`fvs2 mount` /
  `fvs2 unmount`).

## License

MIT. See [LICENSE](LICENSE).
