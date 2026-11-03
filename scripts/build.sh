#!/usr/bin/env bash
# Full local build: frontend, embed, bridge checks, release binaries and the Docker image.
# Usage: scripts/build.sh [--skip-tests]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FRONTEND_DIR="${FRONTEND_DIR:-$ROOT/../spacee-herdr-frontend}"
SKIP_TESTS=0
for arg in "$@"; do
  case "$arg" in
    --skip-tests) SKIP_TESTS=1 ;;
    *) echo "unknown argument: $arg (usage: build.sh [--skip-tests])" >&2; exit 2 ;;
  esac
done

[ -d "$FRONTEND_DIR" ] || { echo "frontend not found at $FRONTEND_DIR (set FRONTEND_DIR)" >&2; exit 1; }
FRONTEND_DIR="$(cd "$FRONTEND_DIR" && pwd)"
VERSION="$(git -C "$ROOT" describe --tags --always --dirty)"
echo "==> building $VERSION"

echo "==> frontend"
cd "$FRONTEND_DIR"
pnpm install --frozen-lockfile
if [ "$SKIP_TESTS" -eq 0 ]; then pnpm ng test --watch=false; fi
pnpm build

echo "==> embed"
SRC="$FRONTEND_DIR/dist/spacee-herdr-frontend/browser"
DEST="$ROOT/internal/webui/dist"
[ -f "$SRC/index.html" ] || { echo "frontend build missing: $SRC/index.html" >&2; exit 1; }
[ -n "$DEST" ] && [ -d "$DEST" ] || { echo "embed dir missing: $DEST" >&2; exit 1; }
find "$DEST" -mindepth 1 -delete
cp -R "$SRC"/. "$DEST"/

echo "==> bridge"
cd "$ROOT"
go vet ./...
if [ "$SKIP_TESTS" -eq 0 ]; then go test ./...; fi

echo "==> binaries"
OUT="$ROOT/bin/release"
mkdir -p "$OUT"
rm -f "$OUT"/spacee-herdr-bridge_* "$OUT"/SHA256SUMS
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" \
    -o "$OUT/spacee-herdr-bridge_${VERSION}_linux_$arch" ./cmd/bridge
done
(cd "$OUT" && sha256sum spacee-herdr-bridge_* > SHA256SUMS)
ls -lh "$OUT"

echo "==> docker image"
docker build --build-arg "VERSION=$VERSION" \
  -t "spacee-herdr-bridge:$VERSION" -t spacee-herdr-bridge:latest "$ROOT"
echo "==> done: $VERSION"
