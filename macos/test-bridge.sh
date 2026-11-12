#!/bin/bash
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
: "${OUT:?Set OUT to a directory for bridge test artifacts}"
: "${FVS2:?Set FVS2 to a built fvs2 CLI}"
mkdir -p "$OUT/source/repo" "$OUT/source/upper" "$OUT/tmp"
OUT=$(cd "$OUT" && pwd)
export TMPDIR="$OUT/tmp" TMP="$OUT/tmp" TEMP="$OUT/tmp"
if [[ -e "$OUT/source/repo/.fvs2" ]]; then
    echo "Use a fresh OUT directory for the bridge fixture." >&2
    exit 1
fi
python3 - "$OUT/source/repo/large.bin" <<'PY'
import sys
from pathlib import Path
Path(sys.argv[1]).write_bytes((bytes(range(251)) * (4 * 1024 * 1024 // 251 + 1))[:4 * 1024 * 1024])
PY
"$FVS2" --path="$OUT/source/repo" init
"$FVS2" --path="$OUT/source/repo" commit -m initial
python3 - "$OUT/source" <<'PY'
import json, sys
from pathlib import Path
source = Path(sys.argv[1])
index = json.loads((source / 'repo/.fvs2/index.json').read_text())
(source / 'fvs-mount.json').write_text(json.dumps({
    'version': 1, 'layers': [{'repository': 'repo', 'state': index['commits'][-1]['id']}], 'upper': 'upper'
}))
PY
cd "$ROOT"
CGO_ENABLED=1 go build -buildmode=c-archive -o "$OUT/libfvs.a" ./cmd/fvsbridge
link_flags=()
if [[ $(uname -s) == Darwin ]]; then
    link_flags=(-framework CoreFoundation -framework Security -lresolv)
fi
${CC:-cc} "${link_flags[@]}" "$ROOT/macos/tests/bridge.c" "$OUT/libfvs.a" -I "$OUT" -lpthread -ldl -lm -o "$OUT/bridge-test"
"$OUT/bridge-test" "$OUT/source"
