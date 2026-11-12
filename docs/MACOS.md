# macOS FSKit backend

The unsigned app and extension compile on macOS 26 ARM in CI. Go race tests
and the C archive executable test pass there too. Signing and the real macOS
mount path remain unverified. Treat this backend as experimental until the
signed mount tests below pass on a prepared Mac. Linux mounts have been
exercised separately.

## Requirements

Mounts require **macOS 26.0 or later** and an installed, enabled FVS filesystem
extension. Building the app requires Xcode with the macOS 26 SDK or later, Go
1.26 and Python 3. Apple Silicon and Intel builds are separate; `ARCH=arm64` and
`ARCH=x86_64` select the target. The ordinary Go CLI build needs no Apple SDK.

FSKit first shipped earlier, but that does not make repository-backed mounts
available on macOS 15.4. FVS uses `FSPathURLResource`, introduced in macOS 26.0.
It represents a directory and transports its security-scoped URL to the
extension. A block-device resource is unsuitable for FVS's repository layout.
`FSGenericURLResource`, also introduced in 26.0, provides an abstract URL but
does not itself grant access to repository directories. FVS uses the path
resource rather than adding a network service or fake block device.

The implementation follows Apple's [passthrough sample](https://developer.apple.com/documentation/fskit/building-a-passthrough-file-system)
and [FSPathURLResource](https://developer.apple.com/documentation/fskit/fspathurlresource)
contract. The extension's `Info.plist` declares `FSSupportsPathURLs`,
`FSRequiresSecurityScopedPathURLResources`, `FSShortName=fvs` and the
`com.apple.fskit.fsmodule` extension point. Its entitlements enable App Sandbox
and [FSKit Module](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.fskit.fsmodule).
No kernel extension is packaged or selected. Recovery Mode, Reduced Security
and SIP changes are not part of installation.

## Build and sign

Clone `core`, `fvs2` and `fvs2d` beside one another to test the local sources:

```sh
go work init ./core ./fvs2 ./fvs2d
go build -trimpath -o "$HOME/artifacts/fvs2" ./fvs2/cmd/fvs2
cd fvs2d
OUT="$HOME/artifacts/fvs-macos" macos/build.sh
```

Without `SIGN_IDENTITY`, this compiles an unsigned `FVS.app` for build checks.
An unsigned build is not an installation or mount verification. The app embeds
`Contents/Extensions/FVS.appex`, the `fvs2d` CLI and the `fvsfsctl` discovery
helper. The extension links the shared Go engine as a C archive. Use a fresh
`OUT` directory for each app build to avoid retaining old signing material.

For distribution, the maintainer needs a Developer ID Application certificate
and its private key in the signing keychain. Configure the container App ID
`lab.fvs.FVS` and extension App ID `lab.fvs.FVS.FS` for the same team. Check that
the signing configuration authorizes the FSKit entitlement. If a provisioning
profile is required by that configuration, export the matching profile from
the developer account or Xcode and pass it as `EXTENSION_PROFILE`:

```sh
SIGN_IDENTITY='Developer ID Application: Your Organization (TEAMID)' \
EXTENSION_PROFILE='/path/to/FVS.provisionprofile' \
OUT="$HOME/artifacts/fvs-macos-signed" macos/build.sh
```

`EXTENSION_PROFILE` is optional when the signing configuration does not need
an embedded profile. The script signs the nested executables and extension
before signing the container. `codesign --verify --deep --strict` checks the
bundle structure; it does not prove entitlement admission or successful mounts.
The signing and provisioning path still needs verification with the maintainer's
actual credentials. Apple's [entitlement diagnostics](https://developer.apple.com/documentation/bundleresources/diagnosing-issues-with-entitlements)
explain how to compare a profile with the signed entitlements.

Notarization is a separate, explicit step. Create a `notarytool` keychain
profile with the maintainer's Apple credentials, then run:

```sh
NOTARY_PROFILE=fvs-notary macos/notarize.sh "$HOME/artifacts/fvs-macos-signed/FVS.app"
```

This submits the signed app to Apple's notary service and staples the ticket.
It does not publish a release. No signing credentials are stored in the repo.

## Install and authorize

Copy the signed, notarized `FVS.app` to `/Applications` or `~/Applications` and
open it once. Enable FVS under **System Settings > General > Login Items &
Extensions > File System Extensions**. This is a user-space filesystem
extension authorization. macOS may also request ordinary access to the source
folder, particularly Documents, Desktop, Downloads or removable storage.
Grant access to the directory selected as the mount source. Installation does
not mean zero authorizations.

Use the CLI inside the bundle, or add its directory to `PATH`:

```sh
export PATH="/Applications/FVS.app/Contents/MacOS:$PATH"
fvs2d -probe
# Also check a running manager and report connection/authentication failures:
fvs2d -probe -control "unix:$HOME/Library/Application Support/FVS/control.sock"
```

For an app installed elsewhere, set `FVS_APP_PATH=/path/to/FVS.app`. A separately
built Go CLI uses the helper from that app. The helper queries
[`FSClient.installedExtensions`](https://developer.apple.com/documentation/fskit/fsclient/fetchinstalledextensions(completionhandler:))
and `FSModuleIdentity.isEnabled`; it does not infer approval from bundle files.

## Mount and unmount

Put the repositories, their block stores and the writable upper below a common
source directory. The mount point must be an empty directory outside that
source. The extension receives access to the source directory through FSKit;
it does not receive arbitrary access to the rest of the home directory.

```sh
mkdir -p "$HOME/FVS/work" "$HOME/mnt/fvs"
# Existing repositories: ~/FVS/base and ~/FVS/app
fvs2d -source "$HOME/FVS" -mount "$HOME/mnt/fvs" \
  -lower "$HOME/FVS/base" -lower "$HOME/FVS/app"

# Add writes, copy-up and whiteout markers in a plain directory:
fvs2d -source "$HOME/FVS" -mount "$HOME/mnt/fvs" \
  -lower "$HOME/FVS/base" -lower "$HOME/FVS/app" -upper "$HOME/FVS/work"

# Snapshot or branch selectors also work with -repo/-state/-branch.
fvs2d -source "$HOME/FVS" -repo "$HOME/FVS/base" -state STATE_ID \
  -mount "$HOME/mnt/fvs"

# From another terminal:
fvs2d -unmount "$HOME/mnt/fvs"
```

The daemon resolves selectors to immutable state IDs and writes `fvs-mount.json`
in the source directory. It pins that configuration before invoking
`/sbin/mount -t fvs SOURCE MOUNTPOINT`, the path-resource mount mechanism shown
by Apple's sample. `/sbin/umount MOUNTPOINT` also unmounts the volume. The Go CLI
waits for the volume to detach; SIGINT and SIGTERM request a normal unmount.
`-source` may be omitted when a sufficiently narrow common directory can be
inferred. An inferred home directory or filesystem root is refused. A source
lock serializes configuration and activation across CLI processes.

The configuration is mount input, not a new FVS repository format. Keep it
outside snapshot repositories, or exclude it with `.fvsignore` if the source is
itself a repository. External block-store paths and symlinks must resolve
inside the authorized source. Upper directories must not overlap lower metadata
or blocks. Mounting over the storage backing the same view is rejected.

The FSKit extension owns its engine and file handles. It does not depend on a
persistent Go daemon for reads, so killing the CLI is expected to leave the
volume usable until an explicit unmount. That expectation must be checked on
macOS by the integration harness. A restarted manager does not adopt earlier
mounts; the standalone `-unmount` command remains available for them.

Manager mode uses the same `CreateMount`/`Unmount` API as Linux. Set
`MountSpec.source_path` when automatic source selection is unsuitable. `Probe`
returns `mount_backend`, `minimum_macos_version`, `mount_backend_available`,
`mount_diagnostic` and `supports_lazy_unmount`. Legacy FUSE fields are false on
macOS. FSKit has no Linux lazy-unmount operation; requests return `Unimplemented`
without silently changing their meaning. Failed normal unmounts remain in the
manager registry so callers can retry.

## Semantics and limits

`internal/engine` owns snapshot resolution, layer precedence, whiteouts,
lookup, metadata, verified offset reads, copy-up and upper mutations. Linux
adapts that engine to go-fuse; Swift adapts FSKit callbacks to the same engine.
Swift contains no FVS format parser or layer-composition algorithm. Metadata
calls use JSON over the C ABI within the process. File reads and writes use
borrowed buffers and numeric handles directly, with no per-read RPC, socket
or helper process. The C ABI has a separate executable test and benchmarks.

Mounting reads metadata, not every file's contents. A lower read fetches only
overlapping blocks and checks their hashes and declared lengths. Opening a
lower for writing copies that file into the upper with a temporary file,
file sync and rename. Existing readers follow the copy-up, while unlinked
open files retain their contents. Directory rename may copy the affected
subtree; that cost occurs on mutation, not on mount or small reads.

Case-sensitive lookup and byte-preserved UTF-8 names are advertised. FVS does
not case-fold or normalize snapshot names. Invalid UTF-8, overlong names and
unrepresentable upper names produce errors. On a case- or normalization-
insensitive upper filesystem, a different existing spelling is rejected with
`EILSEQ` instead of aliasing or overwriting it. Whiteouts match exact spelling too;
creating two markers that the upper cannot distinguish returns `EILSEQ`.
Uppers that rewrite Unicode names
(such as decomposing HFS+ volumes) are refused. Native restore and checkout
preflight the destination's actual case/Unicode behavior and reject collisions.

Stored POSIX permission bits and modification times are exposed. `clear_privileged_bits`
applies to copy-up and metadata changes as well as lower attributes. Directory
metadata is available when the state records it; existing format-1/2/3 states
without directory entries retain inferred directory attributes. For a new test
repository, `fvs2 init --format=4` selects the existing format that records them.
The port does not change existing repository formats or defaults.

The FSKit adapter explicitly rejects FIFOs, hard-link creation, xattrs, ACLs,
BSD flags and unsupported birth/backup timestamps rather than pretending to
preserve them. These are adapter limits, not claims that FSKit cannot implement
them. App bundles needing resource forks or other unrecorded metadata need
additional format support. Symlinks are stored as targets; device nodes are
not exposed. Executable loading, `mmap`, Finder and small writes need the real
macOS tests even though the corresponding Linux tests passed.

FSKit exposes volume synchronization rather than a separate per-file fsync
callback. The adapter syncs open files from its synchronize callback. Upper
namespace changes are not a journaled multi-file transaction; interrupted
extension processes can leave hidden copy-up temporaries or whiteout state.
The lower repository remains immutable. A writable upper is locked for one
mount and must not be modified by external tools while mounted. The manager's
`DiffMount` pruning option must run only while applications have closed their
files and stopped writing. It compares upper content without coordinating
with open file handles. Existing block-store sync and atomic metadata rules
are retained; no new power-loss guarantee is claimed. Do not
run GC or delete mounted states while their blocks are in use.

macFUSE's [FSKit backend](https://github.com/macfuse/macfuse/wiki/FUSE-Backends)
is a viable separate library API, but go-fuse v2.10.1 mounts Darwin devices
through its own [kernel FUSE transport](https://github.com/hanwen/go-fuse/blob/v2.10.1/fuse/mount_darwin.go).
Adding `backend=fskit` to that transport does not integrate macFUSE's mount
framework. This port uses Apple's native extension and has no automatic
kernel-backend fallback.

## Verification

Go unit tests and C ABI tests are separate from native mount tests:

```sh
go test ./core/... ./fvs2/... ./fvs2d/...
go test -race ./fvs2d/internal/engine ./fvs2d/internal/bridge ./fvs2d/cmd/fvs2d
OUT="$HOME/artifacts/fvs-bridge-test" FVS2="$HOME/artifacts/fvs2" \
  fvs2d/macos/test-bridge.sh
```

On a Mac with the signed extension enabled, run the real harness with a fresh
work directory:

```sh
python3 fvs2d/macos/integration.py \
  --daemon /Applications/FVS.app/Contents/MacOS/fvs2d \
  --fvs2 "$HOME/artifacts/fvs2" \
  --work-dir "$HOME/artifacts/fvs-macos-mount-test" --finder
# Also exercise mount lifecycle through the actual gRPC control server:
FVS2D_TEST_REAL_MOUNT=1 go test ./fvs2d/cmd/fvs2d -run TestManagerRealMountLifecycle -count=1
```

It checks content hashes, random/partial reads and EOF, nested directories,
empty files, symlinks, permissions, layers, whiteouts, copy-up, rename,
truncate, deletion, concurrent readers, open files, `mmap`, an executable,
a dynamic library, symlink timestamps and repeated mount/unmount. It also kills its own daemon
and checks FSKit's independent lifetime. Results record mount latency, first
three-byte read, sequential throughput, random-read median/p95, daemon RSS
and visible FVS extension RSS. No upper data files may appear after mount or
the first small read; a reserved zero-byte lock file is mount metadata.
`--finder` requires a human to inspect browsing and preview and confirm the
result. Without it, Finder is marked not verified.

Extension-crash testing is separate: identify the PID of the test volume's
FVS extension, verify its command and ownership, terminate only that PID,
check that requests fail without corrupting lower blocks, unmount and mount
again. The harness does not kill processes selected only by name, because
an extension may serve another user's test volume. Power-loss recovery is
not simulated by killing the daemon.

CI compiles the unsigned Swift app on macOS 26 ARM and runs Go and
C tests on Linux/macOS. This is not a mount claim. The manually dispatched
`Real FSKit mount` workflow requires a prepared self-hosted runner labeled `fvs-fskit`,
with a signing identity, optional profile and extension approval already set
up. `FVS_SIGN_IDENTITY` and `FVS_EXTENSION_PROFILE_PATH` are runner/repository
configuration, not secrets committed to source. No CI job changes kernel
security settings. Only validated `v*` tags publish release assets; ordinary
pushes and PRs retain builds as Actions artifacts.

Current local evidence: Linux Go suites, vet, race checks, C ABI lifecycle and
real Linux mounts passed, including gRPC mount/remount/external unmount. The
final three Linux harness runs measured 52-53 ms to mount, 0.88-1.17 ms for the
first three-byte read, 115-125 MiB/s for a warm sequential hash pass, random-read
medians of 203-230 us (p95 621-671 us), and about 15 MB initial daemon RSS.
The final C ABI upper 4 KiB test measured about 2.1 us/read (10,000 calls).
Earlier C ABI samples ranged from 0.8 to 7.7 us/read with machine load. These are development-machine samples,
not macOS or cold-cache performance claims. The
[macOS CI run](https://github.com/fvs-lab/fvs2d/actions/runs/43210344505)
compiled the unsigned app and extension and passed Go race tests, vet and the
C archive executable test. Its downloaded archive passed SHA-256, bundle
metadata and ARM64 executable checks. Signing, notarization, Finder and
actual FSKit mounts remain unverified.
