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

# run the persistent gRPC mount manager over a Unix socket (default, no token needed)
fvs2d -control unix:/run/user/1000/fvs2d.sock

# manager over loopback TCP for local dev (no token required, but a warning is logged)
fvs2d -control tcp:127.0.0.1:50071

# manager over TCP reachable from other hosts: requires both --insecure-tcp and --token
fvs2d -control tcp:0.0.0.0:50071 -insecure-tcp -token "$(openssl rand -hex 32)"
# (or export FVS2D_TOKEN instead of -token; clients send it back as the
# "x-fvs2d-token" gRPC metadata header, or "authorization: Bearer <token>")

# sandboxed: reject any client-supplied path outside the given root
fvs2d -control unix:/run/user/1000/fvs2d.sock -root /home/user/bottles-next
```

Direct mounts can be unmounted with `fusermount3 -u /mnt/state` or by stopping
the daemon. Manager-owned mounts are created, inspected and unmounted through
the `fvs2d.v1.Fvs2d` gRPC API.

### Manager flags

- `-control` : `unix:/path.sock` or `tcp:host:port` (empty disables the manager).
- `-root`    : allowed-root sandbox; every client-supplied `repository_path`,
  `destination_path`, mount layer path and upper-layer path must resolve
  inside it. Omit to keep serving arbitrary filesystem paths (the daemon logs
  one prominent startup warning when unset). Reported back to clients as
  `ProbeResponse.work_dir`.
- `-insecure-tcp` : required, together with `-token`, to bind a non-loopback
  TCP address. Loopback TCP (127.0.0.1/::1) never needs it.
- `-token` : shared control-API token for TCP, compared in constant time
  against the `x-fvs2d-token` (or `authorization: Bearer`) gRPC metadata
  header. Defaults to `$FVS2D_TOKEN`. Unix sockets rely on filesystem
  permissions instead and never require a token.

## Status and roadmap

- Read-only mounts of a single state or a stack of layers work today.
- A writable upper layer (`-upper`) works today: writes, creates, deletes and
  copy-ups are recorded in the upper directory; deletes are stored as
  `.wh.<name>` whiteout markers.
- Manager mode (`-control` without `-mount`) exposes standard gRPC health plus
  `Probe`, `InitRepository`, `Commit`, `CommitStream`, `ListCommits`,
  `GetCommit`, `Restore`, `RestoreStream`, `ListFiles`, `GetFile`, `Diff`,
  `CreateMount`, `GetMount`, `ListMounts`, `Unmount` and `Shutdown`.

## License

MIT. See [LICENSE](LICENSE).
