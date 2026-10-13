#!/usr/bin/env bash
# Builds a release version of fulcrum, the same way the CI release job does:
# a static binary without debug information, stamped with its version.
#
# Usage:
#
#   scripts/build.sh [--all] [--version <version>]
#
#   --all                build the three release targets (windows/amd64,
#                        linux/amd64, darwin/arm64) into dist/, with a
#                        SHA256SUMS file, instead of this machine's into bin/
#   --version <version>  version reported by "fulcrum -version"
#                        (default: git describe, such as v0.3.2 or
#                        v0.3.2-3-g1a2b3c4-dirty)
set -euo pipefail

all=false
version=""
while [ $# -gt 0 ]; do
  case "$1" in
    --all) all=true ;;
    --version)
      [ $# -ge 2 ] || { echo "error: --version needs a value" >&2; exit 2; }
      version="$2"
      shift
      ;;
    -h | --help)
      sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 2
      ;;
  esac
  shift
done

# Run from the repository root, wherever the script is called from.
cd "$(dirname "$0")/.."

if [ -z "$version" ]; then
  version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

# build <goos> <goarch> <output file>
build() {
  echo "building $3 ($1/$2, $version)"
  GOOS="$1" GOARCH="$2" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$3" ./src/cmd/fulcrum
}

if "$all"; then
  rm -rf dist
  mkdir -p dist
  build windows amd64 dist/fulcrum-windows-amd64.exe
  build linux amd64 dist/fulcrum-linux-amd64
  build darwin arm64 dist/fulcrum-darwin-arm64
  if command -v sha256sum >/dev/null 2>&1; then
    (cd dist && sha256sum fulcrum-* >SHA256SUMS)
  else
    (cd dist && shasum -a 256 fulcrum-* >SHA256SUMS)
  fi
  echo "wrote dist/SHA256SUMS"
else
  goos="$(go env GOOS)"
  ext=""
  [ "$goos" = windows ] && ext=".exe"
  build "$goos" "$(go env GOARCH)" "bin/fulcrum$ext"
fi
