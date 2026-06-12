#!/bin/sh
# Stacks — one-line installer. Grabs the latest COMPILED binary and sets up
# Docker (if missing) + the boot/watchdog services. No source, nothing to edit.
#
#   curl -fsSL https://raw.githubusercontent.com/crunkazcanbe/stacks-extended/master/install.sh | sh
#
set -e
REPO="crunkazcanbe/stacks-extended"
DEST="/usr/local/bin/stacks"
case "$(uname -m)" in
  x86_64|amd64)  ASSET="stacks-linux-amd64" ;;
  aarch64|arm64) ASSET="stacks-linux-arm64" ;;
  *) echo "unsupported arch: $(uname -m)"; exit 1 ;;
esac
echo "▸ fetching latest stacks ($ASSET)…"
URL=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep -oE "https://[^\"]+/$ASSET" | head -1)
[ -z "$URL" ] && { echo "✘ no release asset '$ASSET' found"; exit 1; }
SUDO=""; [ "$(id -u)" -ne 0 ] && SUDO="sudo"
$SUDO curl -fsSL -o "$DEST" "$URL"
$SUDO chmod +x "$DEST"
echo "✔ installed $("$DEST" version 2>/dev/null || echo stacks)"
echo "▸ setting up Docker (if needed) + boot/watchdog services…"
$SUDO "$DEST" install
echo "✔ done — try:  stacks menu"
