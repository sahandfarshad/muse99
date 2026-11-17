#!/usr/bin/env bash
# Muse99 — uninstaller.
#
# Removes the binary and systemd service files. Your data in
# ~/.muse99 (config, chats, memory, usage) is KEPT by default.
#
# Usage:
#   bash uninstall.sh            # asks first, keeps data unless you say yes
#   bash uninstall.sh --yes      # no prompts, removes data too
#   INSTALL_DIR=$HOME/bin bash uninstall.sh
#
# You can also uninstall from inside Muse99 itself:
#   muse99 --uninstall           # prompts
#   muse99 --uninstall --yes     # no prompts, removes data too
#   or SSH menu option 16 (Uninstall Muse99).
set -uo pipefail

BIN="${INSTALL_DIR:-$HOME/.local/bin}/muse99"
DATA_DIR="$HOME/.muse99"
YES=0
for a in "$@"; do
  case "$a" in
    --yes) YES=1 ;;
    *) echo "Unknown option: $a (only --yes is supported)" >&2; exit 1 ;;
  esac
done

echo "Uninstall Muse99 — this will remove:"
[ -f "$BIN" ] && echo "  - binary: $BIN"
[ -f "$HOME/.config/systemd/user/muse99.service" ] && echo "  - service: $HOME/.config/systemd/user/muse99.service"
[ -f "/etc/systemd/system/muse99.service" ] && echo "  - service: /etc/systemd/system/muse99.service"
[ -f "$DATA_DIR/muse99.service" ] && echo "  - service: $DATA_DIR/muse99.service"
echo "  (kept) data: $DATA_DIR (config, chats, memory, usage)"
echo ""

REMOVE_DATA=0
if [ "$YES" = 1 ]; then
  REMOVE_DATA=1
else
  printf "Type YES to uninstall Muse99: "
  read -r ans || { echo "Uninstall cancelled — nothing was removed."; exit 0; }
  [ "$ans" = "YES" ] || { echo "Uninstall cancelled — nothing was removed."; exit 0; }
  printf "Also delete your Muse99 data (%s)? [y/N]: " "$DATA_DIR"
  read -r ans2 || ans2=""
  case "$ans2" in y|Y|yes|YES) REMOVE_DATA=1 ;; esac
fi

# Stop the web UI first (PID left by the installer / web UI).
if [ -f "$DATA_DIR/web.pid" ]; then
  PID="$(cat "$DATA_DIR/web.pid" 2>/dev/null || true)"
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "$PID" 2>/dev/null || break; sleep 0.1; done
    kill -9 "$PID" 2>/dev/null || true
    echo "Stopped the web UI (PID $PID)."
  fi
  rm -f "$DATA_DIR/web.pid" "$DATA_DIR/web.log"
fi

REMOVED=0
if [ -f "$BIN" ] || [ -L "$BIN" ]; then rm -f "$BIN" && echo "Removed: $BIN" && REMOVED=1; fi
for svc in "$HOME/.config/systemd/user/muse99.service" "/etc/systemd/system/muse99.service" "$DATA_DIR/muse99.service"; do
  if [ -f "$svc" ]; then
    if rm -f "$svc" 2>/dev/null; then
      echo "Removed: $svc"
      case "$svc" in
        /etc/*) echo "  (system service removed — run 'sudo systemctl daemon-reload' if it still shows up)" ;;
        *) systemctl --user daemon-reload 2>/dev/null || true ;;
      esac
    else
      echo "Could not remove $svc (no permission?) — remove it with sudo if needed."
    fi
  fi
done
if [ "$REMOVE_DATA" = 1 ]; then
  rm -rf "$DATA_DIR" && echo "Removed data: $DATA_DIR"
else
  echo "Kept data: $DATA_DIR"
fi
# Remove the install dir only if it is now empty.
INSTALL_DIR_ACTUAL="$(dirname "$BIN")"
rmdir "$INSTALL_DIR_ACTUAL" 2>/dev/null && echo "Removed empty install dir: $INSTALL_DIR_ACTUAL" || true

echo ""
echo "Muse99 uninstalled."
echo "To install again: curl -fsSL https://raw.githubusercontent.com/sahfar363/muse99/main/install.sh | bash"
