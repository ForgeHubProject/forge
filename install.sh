#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="/usr/bin"
BINARY_NAME="forge"
CMD_PATH="./cmd/forge"

# ── platform detection ────────────────────────────────────────────────────────

OS="$(uname -s)"
ARCH="$(uname -m)"

case "$OS" in
  Linux)             GOOS="linux" ;;
  Darwin)            GOOS="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
  *)
    echo "error: unsupported OS: $OS" >&2
    echo "       forge install supports Linux, macOS, and Windows (via Git Bash/MSYS/WSL)." >&2
    exit 1
    ;;
esac

case "$ARCH" in
  x86_64)          GOARCH="amd64" ;;
  aarch64|arm64)   GOARCH="arm64" ;;
  armv7l)          GOARCH="arm" ;;
  i386|i686)       GOARCH="386" ;;
  *)
    echo "error: unsupported architecture: $ARCH" >&2
    exit 1
    ;;
esac

echo "Platform: $GOOS/$GOARCH"

# ── macOS install path ────────────────────────────────────────────────────────

if [ "$GOOS" = "darwin" ]; then
  if command -v brew &>/dev/null; then
    INSTALL_DIR="$(brew --prefix)/bin"
  else
    INSTALL_DIR="/usr/local/bin"
  fi
fi

# ── Windows (Git Bash/MSYS) install path ─────────────────────────────────────

if [ "$GOOS" = "windows" ]; then
  BINARY_NAME="forge.exe"
  # No sudo on Windows; install to a user-writable dir instead of /usr/bin.
  INSTALL_DIR="$HOME/bin"
  mkdir -p "$INSTALL_DIR"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "note: add $INSTALL_DIR to PATH to use 'forge' from any shell." ;;
  esac
fi

# ── prerequisites ─────────────────────────────────────────────────────────────

if ! command -v go &>/dev/null; then
  echo "error: Go is not installed or not in PATH." >&2
  echo "       Install Go from https://go.dev/dl/ then re-run this script." >&2
  exit 1
fi

GO_VERSION="$(go version | awk '{print $3}' | sed 's/go//')"
echo "Go:      $GO_VERSION"

# Verify we're in the forge repo root
if [ ! -f "go.mod" ] || ! grep -q '^module github.com/forgehubproject/forge$' go.mod 2>/dev/null; then
  echo "error: run this script from the forge repository root." >&2
  exit 1
fi

# ── build ─────────────────────────────────────────────────────────────────────

BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT

OUTFILE="$BUILD_DIR/$BINARY_NAME"

echo "Building forge..."
GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -o "$OUTFILE" "$CMD_PATH"

echo "Built: $OUTFILE ($(du -sh "$OUTFILE" | cut -f1))"

# ── install ───────────────────────────────────────────────────────────────────

DEST="$INSTALL_DIR/$BINARY_NAME"

if [ ! -w "$INSTALL_DIR" ]; then
  echo "Installing to $DEST (requires sudo)..."
  sudo install -m 755 "$OUTFILE" "$DEST"
else
  install -m 755 "$OUTFILE" "$DEST"
fi

echo "Installed: $DEST"
"$DEST" --version 2>/dev/null || "$DEST" --help 2>&1 | head -1
echo "Done. Run 'forge --help' to get started."
