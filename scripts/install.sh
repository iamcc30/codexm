#!/usr/bin/env sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
INSTALL_DIR=${CODEXM_INSTALL_DIR:-"$HOME/.local/bin"}
VERSION=${CODEXM_VERSION:-"dev"}

if ! command -v go >/dev/null 2>&1; then
  echo "Go 1.22+ is required to build codexm from source." >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
cd "$ROOT_DIR"
go test ./...
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$INSTALL_DIR/codexm" ./cmd/codexm
chmod 755 "$INSTALL_DIR/codexm"

echo "Installed codexm to $INSTALL_DIR/codexm"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "Add $INSTALL_DIR to your PATH." ;;
esac
