#!/usr/bin/env sh
set -eu

VERSION=${1:-dev}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
DIST_DIR="$ROOT_DIR/dist"

rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"
cd "$ROOT_DIR"
go test ./...

build() {
  GOOS_VALUE=$1
  GOARCH_VALUE=$2
  EXT=$3
  NAME="codexm_${VERSION}_${GOOS_VALUE}_${GOARCH_VALUE}"
  STAGE="$DIST_DIR/$NAME"
  mkdir -p "$STAGE"
  echo "Building $GOOS_VALUE/$GOARCH_VALUE"
  CGO_ENABLED=0 GOOS=$GOOS_VALUE GOARCH=$GOARCH_VALUE \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$STAGE/codexm$EXT" ./cmd/codexm
  cp README.md README.zh-CN.md LICENSE "$STAGE/"
  if [ "$GOOS_VALUE" = "windows" ]; then
    (cd "$DIST_DIR" && zip -qr "$NAME.zip" "$NAME")
  else
    (cd "$DIST_DIR" && tar -czf "$NAME.tar.gz" "$NAME")
  fi
  rm -rf "$STAGE"
}

build linux amd64 ""
build linux arm64 ""
build darwin amd64 ""
build darwin arm64 ""
build windows amd64 ".exe"
build windows arm64 ".exe"

cd "$DIST_DIR"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum ./*.tar.gz ./*.zip > checksums.txt
else
  shasum -a 256 ./*.tar.gz ./*.zip > checksums.txt
fi

echo "Artifacts written to $DIST_DIR"
