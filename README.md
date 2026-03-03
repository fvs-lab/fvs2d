# fvs2d (FUSE daemon)

FVS v2 daemon exposes branches via FUSE.

## Build

Default build (no FUSE backend):

```bash
go build ./cmd/fvs2d
```

FUSE3 build (requires libfuse3 development headers/libs):

```bash
go build -tags fuse3 ./cmd/fvs2d
```
