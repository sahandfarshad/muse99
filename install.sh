#!/usr/bin/env bash
# Muse99 — one-line installer for Ubuntu server.
#
# Usage (after you push this repo to GitHub):
#   curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash
#
# Or from a clone:
#   ./install.sh
#
# It installs Go 1.23 if missing, builds Muse99, and puts `muse99` in ~/.local/bin.
# Env overrides:
#   REPO_URL   - git repo to clone, default https://github.com/sahfar363/muse99.git
#   INSTALL_DIR - where to put the binary, default $HOME/.local/bin
#   GO_VERSION - default 1.23.0
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/sahfar363/muse99.git}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
GO_VERSION="${GO_VERSION:-1.23.0}"
SRC_DIR="${SRC_DIR:-$HOME/muse99}"

echo "==> Muse installer"

# --- Go ---
if command -v go >/dev/null 2>&1; then
  echo "Go already installed: $(go version)"
else
  echo "Go not found — installing Go $GO_VERSION..."
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64) GO_ARCH=amd64 ;;
    aarch64|arm64) GO_ARCH=arm64 ;;
    *) echo "Unsupported arch: $ARCH"; exit 1 ;;
  esac
  TMP="$(mktemp -d)"
  curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz" -o "$TMP/go.tar.gz"
  # Prefer /usr/local/go with sudo, fall back to $HOME/go-tool without it
  if [ -w /usr/local ] || sudo -n true 2>/dev/null; then
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$TMP/go.tar.gz"
    export PATH="/usr/local/go/bin:$PATH"
  else
    mkdir -p "$HOME/go-tool"
    tar -C "$HOME/go-tool" -xzf "$TMP/go.tar.gz"
    export PATH="$HOME/go-tool/go/bin:$PATH"
  fi
  rm -rf "$TMP"
  echo "Installed: $(go version)"
fi

# --- Source ---
if [ -f "./go.mod" ] && grep -q "^module " ./go.mod 2>/dev/null; then
  SRC_DIR="$PWD"
  echo "Building from current directory: $SRC_DIR"
elif [ -d "$SRC_DIR/.git" ]; then
  echo "Updating existing checkout: $SRC_DIR"
  git -C "$SRC_DIR" pull --ff-only || true
else
  echo "Cloning $REPO_URL -> $SRC_DIR"
  git clone "$REPO_URL" "$SRC_DIR"
fi

cd "$SRC_DIR"
go vet ./...
go test ./... || true
go build -o muse99 .

mkdir -p "$INSTALL_DIR"
install -m 755 muse99 "$INSTALL_DIR/muse99"

echo ""
echo "Installed: $INSTALL_DIR/muse99"
echo ""
echo "Next steps:"
echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
echo "  export MODEL_API_KEY=\"your-key\""
echo "  export MODEL_API_BASE_URL=\"https://api.meta.ai/v1\""
echo "  export MODEL_ID=\"muse-spark-1.3\"   # or muse-spark-1.4 when it ships"
echo "  muse99 --allow-shell"
echo ""
echo "Or with any other OpenAI-compatible API, change MODEL_API_BASE_URL / MODEL_ID."
