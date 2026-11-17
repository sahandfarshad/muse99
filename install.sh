#!/usr/bin/env bash
# Muse99 — quick installer for Ubuntu server.
#
# Downloads the pre-built binary from the latest GitHub release.
# No Go, no clone, no compile on the server.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash
#
# Specific version (a git tag, e.g. v0.1.0):
#   curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash -s -- --version v0.1.0
#   VERSION=v0.1.0 bash install.sh
#
# Env overrides:
#   INSTALL_DIR - where to put the binary, default $HOME/.local/bin
#   VERSION     - release tag to download, default: latest
set -euo pipefail

REPO="sahfar363/muse99"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERSION:-}"

# Parse --version (also accepts --version=v0.1.0).
while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      VERSION="${2:-}"; shift 2 ;;
    --version=*)
      VERSION="${1#*=}"; shift ;;
    --install-dir)
      INSTALL_DIR="${2:-}"; shift 2 ;;
    --install-dir=*)
      INSTALL_DIR="${1#*=}"; shift ;;
    *)
      echo "Unknown option: $1" >&2
      echo "Usage: install.sh [--version v0.1.0] [--install-dir DIR]" >&2
      exit 1 ;;
  esac
done

echo "==> Muse99 quick installer (no compile — binary download)"

# --- Architecture ---
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  GO_ARCH=amd64 ;;
  aarch64|arm64) GO_ARCH=arm64 ;;
  *)
    echo "Unsupported architecture: $ARCH (only x86_64/amd64 and aarch64/arm64 releases exist)" >&2
    exit 1 ;;
esac
echo "Architecture: $ARCH ($GO_ARCH)"

if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required. Install it with: sudo apt update && sudo apt install -y curl" >&2
  exit 1
fi

# --- Download URL ---
if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/muse99-linux-${GO_ARCH}"
  echo "Release: latest"
else
  # Accept both "0.1.0" and "v0.1.0".
  case "$VERSION" in
    v*) TAG="$VERSION" ;;
    *)  TAG="v$VERSION" ;;
  esac
  URL="https://github.com/${REPO}/releases/download/${TAG}/muse99-linux-${GO_ARCH}"
  echo "Release: $TAG"
fi
echo "Downloading: $URL"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

if ! curl -fsSL -o "$TMP/muse99" "$URL"; then
  echo "" >&2
  echo "ERROR: could not download Muse99 from:" >&2
  echo "  $URL" >&2
  echo "" >&2
  echo "That usually means no release exists yet for this version." >&2
  echo "Create one first (from your laptop, in the repo):" >&2
  echo "  git tag v0.1.0" >&2
  echo "  git push origin v0.1.0" >&2
  echo "Then wait a minute for GitHub Actions to build and publish" >&2
  echo "muse99-linux-amd64 and muse99-linux-arm64, and run this installer again." >&2
  echo "This installer does not fall back to compiling — the binary is the point." >&2
  exit 1
fi

chmod +x "$TMP/muse99"

# --- Verify the binary actually runs before installing it ---
if ! "$TMP/muse99" --help >/dev/null 2>&1; then
  echo "ERROR: the downloaded file does not run as muse99. Aborting." >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"
install -m 755 "$TMP/muse99" "$INSTALL_DIR/muse99"

echo ""
echo "Installed: $INSTALL_DIR/muse99"
echo ""
echo "Next steps:"
echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
echo "  export MODEL_API_KEY=\"your-key\"            # Spark key from dev.meta.ai"
echo "  export MODEL_API_BASE_URL=\"https://api.meta.ai/v1\""
echo "  export MODEL_ID=\"muse-spark-1.3\"   # or muse-spark-1.4 when it ships"
echo "  muse99 --allow-shell"
echo ""
echo "Other APIs (Gemini, OpenAI, OpenRouter, Ollama, …):"
echo "  run muse99, choose 9 in the SSH menu (Providers / API keys),"
echo "  or set MODEL_API_BASE_URL / MODEL_ID for that provider."
