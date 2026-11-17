#!/usr/bin/env bash
# Muse99 — quick installer for Ubuntu server.
#
# Downloads the pre-built binary from the latest GitHub release.
# No Go, no clone, no compile on the server.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash
#
# After installing, the installer starts the web UI (default port 3759)
# and opens the SSH menu — the web UI link is on top of the menu.
# No terminal setup is needed: your API key is set inside the web UI,
# in Settings → Provider & model. If there is no terminal (CI, a pipe
# without a TTY), it starts the web UI in the background, prints the
# address, and exits instead of hanging.
#
# Specific version (a git tag, e.g. v0.1.0):
#   curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash -s -- --version v0.1.0
#   VERSION=v0.1.0 bash install.sh
#
# Flags: --version TAG | --install-dir DIR | --no-menu | --no-web
#
# Env overrides:
#   INSTALL_DIR - where to put the binary, default $HOME/.local/bin
#   VERSION     - release tag to download, default: latest
#   MUSE99_NO_MENU=1 - do not open the SSH menu after installing
#   MUSE99_NO_WEB=1  - do not start the web UI after installing
#   LOCAL_BINARY - dev/test only: install this local file instead of downloading
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
    --no-menu)
      MUSE99_NO_MENU=1; shift ;;
    --no-web)
      MUSE99_NO_WEB=1; shift ;;
    *)
      echo "Unknown option: $1" >&2
      echo "Usage: install.sh [--version v0.1.0] [--install-dir DIR] [--no-menu] [--no-web]" >&2
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

if [ -n "${LOCAL_BINARY:-}" ]; then
  echo "Using local binary: $LOCAL_BINARY"
  cp "$LOCAL_BINARY" "$TMP/muse99" || { echo "ERROR: could not copy $LOCAL_BINARY" >&2; exit 1; }
elif ! curl -fsSL -o "$TMP/muse99" "$URL"; then
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
BIN="$INSTALL_DIR/muse99"

# --- Start (or restart) the web UI automatically (default port 3759) ---
# A port already saved in ~/.muse99/config.json wins over the default.
# v7: an already-running web UI is RESTARTED, not left alone — the
# old process would keep serving the old binary forever, so an
# update would never take effect until someone stopped it by hand.
PORT=3759
if [ -f "$HOME/.muse99/config.json" ]; then
  SAVED="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('port') or 3759)" "$HOME/.muse99/config.json" 2>/dev/null || echo 3759)"
  case "$SAVED" in ''|*[!0-9]*) ;; *) PORT="$SAVED" ;; esac
fi
LOG="$HOME/.muse99/web.log"
mkdir -p "$HOME/.muse99"
WEB_ADDR="http://localhost:$PORT"
if [ -z "${MUSE99_NO_WEB:-}" ]; then
  RESTARTED=0
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/api/status" >/dev/null 2>&1; then
    echo "Web UI is already running on port $PORT — restarting it with the new version…"
    if [ -f "$HOME/.muse99/web.pid" ]; then
      OLDPID="$(cat "$HOME/.muse99/web.pid" 2>/dev/null || true)"
      case "$OLDPID" in ''|*[!0-9]*) ;; *)
        kill "$OLDPID" 2>/dev/null || true
        for _ in $(seq 1 20); do kill -0 "$OLDPID" 2>/dev/null || break; sleep 0.25; done
        kill -9 "$OLDPID" 2>/dev/null || true
        ;;
      esac
      rm -f "$HOME/.muse99/web.pid"
    else
      pkill -f "$BIN --menu=false --web" 2>/dev/null || true
      sleep 1
    fi
    RESTARTED=1
  fi
  nohup "$BIN" --menu=false --web >"$LOG" 2>&1 &
  # Wait for the "Muse99 web UI: <address>" line (up to ~15s).
  for _ in $(seq 1 30); do
    if grep -q "Muse99 web UI:" "$LOG" 2>/dev/null; then break; fi
    sleep 0.5
  done
  FOUND="$(grep -m1 "Muse99 web UI:" "$LOG" 2>/dev/null | sed 's/.*Muse99 web UI: //; s/ (TLS)//')"
  if [ -n "$FOUND" ]; then WEB_ADDR="$FOUND"; fi
  if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/api/status" >/dev/null 2>&1; then
    if [ "$RESTARTED" = 1 ]; then
      echo "Web UI restarted: $WEB_ADDR"
    else
      echo "Web UI started: $WEB_ADDR"
    fi
  else
    echo "WARNING: the web UI did not start on port $PORT — the port may already be in use." >&2
    echo "Check the log: $LOG" >&2
    echo "You can change the port in the SSH menu (option 5) and start it with option 1." >&2
  fi
fi
echo ""
echo "Open the web UI: $WEB_ADDR"
echo "Set up your API key there: Settings → Provider & model."
echo "No terminal setup needed — everything is in the web UI."
echo ""

# --- Open the SSH menu (the web UI link is on top of it) ---
# When run as `curl … | bash`, stdin is the download pipe, not the
# terminal — so the menu is reopened on /dev/tty when one exists.
# With no terminal at all (CI, piped runs), print the address and exit.
# v7: also print the exact menu command (with its full path when
# ~/.local/bin is not on PATH yet, which is common on a fresh
# server) — "the menu never opened" usually turns out to be that
# `muse99` was not on PATH after the install.
if command -v muse99 >/dev/null 2>&1; then
  MENU_CMD="muse99"
else
  MENU_CMD="$BIN"
  echo "Note: $HOME/.local/bin is not on your PATH yet, so the plain"
  echo "\`muse99\` command will not be found. Fix that with:"
  echo "  export PATH=\"\$HOME/.local/bin:\$PATH\""
  echo "(Add that line to ~/.bashrc to keep it.)"
fi
echo "To open the SSH menu, run: $MENU_CMD"
if [ -z "${MUSE99_NO_MENU:-}" ]; then
  if [ -t 0 ]; then
    exec "$BIN"
  elif [ -e /dev/tty ] && (exec 0</dev/tty) 2>/dev/null; then
    exec "$BIN" </dev/tty
  else
    echo "No terminal attached — the web UI is running in the background."
    echo "Run \`$MENU_CMD\` later to open the SSH menu."
  fi
fi
