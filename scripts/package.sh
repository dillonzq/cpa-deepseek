#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "Usage: $0 VERSION [OUTPUT_DIR]" >&2
  exit 1
fi
version="${1#v}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "Invalid version: $1" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${2:-$root/dist}"
mkdir -p "$output"
output="$(cd "$output" && pwd)"
cd "$root"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"
case "$target_os" in
  linux) extension=so ;;
  darwin) extension=dylib ;;
  windows) extension=dll ;;
  *) echo "Unsupported operating system: $target_os" >&2; exit 1 ;;
esac

package="cpa-deepseek_${version}_${target_os}_${target_arch}"
staging="$(mktemp -d "$output/.package.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
  -ldflags "-s -w -X github.com/dillonzq/cpa-deepseek/internal/plugin.pluginVersion=$version" \
  -o "$staging/cpa-deepseek.$extension" .
cp README.md README.zh-CN.md LICENSE config.example.yaml "$staging/"
cp -R assets "$staging/assets"

# Use the same zip layout on every platform without requiring zip.exe.
python=python3
if ! command -v "$python" >/dev/null 2>&1; then python=python; fi
"$python" - "$staging" "$package" "$output" <<'PY'
import pathlib
import sys
import zipfile

staging, package, output = sys.argv[1:]
with zipfile.ZipFile(pathlib.Path(output) / (package + ".zip"), "w", zipfile.ZIP_DEFLATED) as archive:
    for path in sorted(pathlib.Path(staging).rglob("*")):
        if path.is_file():
            archive.write(path, path.relative_to(staging).as_posix())
PY
