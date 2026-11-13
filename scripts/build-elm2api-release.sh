#!/usr/bin/env bash
# Build the standalone elm2api release artifacts (Windows + Linux amd64 only).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

DIST_DIR="${DIST_DIR:-$ROOT_DIR/dist}"
STAGE_DIR="${STAGE_DIR:-$ROOT_DIR/.tmp/elm2api-stage}"
TEMPLATE_DIR="$ROOT_DIR/scripts/elm2api-release"
PKG_PARENT="$STAGE_DIR/pkg"

mkdir -p "$DIST_DIR"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR/bin" "$PKG_PARENT"

echo "==> go build"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$STAGE_DIR/bin/elm2api.exe" ./cmd/elm2api
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$STAGE_DIR/bin/elm2api-linux-amd64" ./cmd/elm2api

echo "==> windows package"
WIN_PKG="$PKG_PARENT/elm2api-windows-amd64"
mkdir -p "$WIN_PKG"
cp "$STAGE_DIR/bin/elm2api.exe" "$WIN_PKG/elm2api.exe"
cp "$TEMPLATE_DIR/config.json" "$WIN_PKG/config.json"
cp "$TEMPLATE_DIR/config.json" "$WIN_PKG/config.example.json"
cp "$TEMPLATE_DIR/README.md" "$WIN_PKG/README.md"
cp "$TEMPLATE_DIR/start.bat" "$WIN_PKG/start.bat"
rm -f "$DIST_DIR/elm2api-windows-amd64.zip"
tar --format zip -c -f "$DIST_DIR/elm2api-windows-amd64.zip" -C "$PKG_PARENT" elm2api-windows-amd64
cp "$STAGE_DIR/bin/elm2api.exe" "$DIST_DIR/elm2api-windows-amd64.exe"

echo "==> linux package"
PKG="$PKG_PARENT/elm2api-linux-amd64"
mkdir -p "$PKG"
cp "$STAGE_DIR/bin/elm2api-linux-amd64" "$PKG/elm2api"
cp "$TEMPLATE_DIR/config.json" "$PKG/config.example.json"
cp "$TEMPLATE_DIR/README.md" "$PKG/README.md"
rm -f "$DIST_DIR/elm2api-linux-amd64.tar.gz"
tar -czf "$DIST_DIR/elm2api-linux-amd64.tar.gz" -C "$PKG_PARENT" elm2api-linux-amd64
cp "$STAGE_DIR/bin/elm2api-linux-amd64" "$DIST_DIR/elm2api-linux-amd64"

echo "==> standalone source bundle"
SRC="$STAGE_DIR/elm2api-src"
mkdir -p "$SRC/cmd" "$SRC/internal"
cp -R cmd/elm2api "$SRC/cmd/"
cp -R internal/elmcore "$SRC/internal/"
cp "$TEMPLATE_DIR/README.md" "$SRC/README.md"
cp "$TEMPLATE_DIR/config.json" "$SRC/config.example.json"
{
  echo "module elm2api"
  echo
  echo "go 1.26.0"
} > "$SRC/go.mod"
printf '\n## 从源码构建\n\ngo build -o elm2api ./cmd/elm2api\n' >> "$SRC/README.md"
( cd "$SRC" && go vet ./... && go test ./... )
rm -f "$DIST_DIR/elm2api-src.zip"
tar --format zip -c -f "$DIST_DIR/elm2api-src.zip" -C "$STAGE_DIR" elm2api-src

echo "==> done"
ls -lh "$DIST_DIR"/elm2api-*
