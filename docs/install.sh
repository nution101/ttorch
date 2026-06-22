#!/bin/sh
# orcha installer (macOS / Linux / WSL).
#   curl -fsSL https://nution101.github.io/orcha/install.sh | sh
set -eu

REPO="${ORCHA_REPO:-nution101/orcha}"
INSTALL_DIR="${ORCHA_HOME:-$HOME/.orcha}/bin"
BIN_PATH="$INSTALL_DIR/orcha"

# Choose a PATH dir we can link into.
if [ -n "${ORCHA_BIN_DIR:-}" ]; then
  LINK_DIR="$ORCHA_BIN_DIR"
else
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) LINK_DIR="$HOME/.local/bin" ;;
    *) LINK_DIR="/usr/local/bin" ;;
  esac
fi
LINK_PATH="$LINK_DIR/orcha"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$OS" in
  darwin | linux) ;;
  *) echo "Unsupported OS: $OS (on Windows, run this inside WSL2)"; exit 1 ;;
esac
case "$ARCH" in
  x86_64 | amd64) ARCH="amd64" ;;
  arm64 | aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
  | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
if [ -z "$VERSION" ]; then
  echo "Could not determine latest release for ${REPO}."
  echo "If there is no release yet, build from source: 'make install'."
  exit 1
fi

ASSET="orcha-${VERSION}-${OS}-${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading orcha ${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}"
curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt"

# Verify sha256.
EXPECTED="$(grep " \*\{0,1\}${ASSET}\$" "${TMP}/checksums.txt" | awk '{print $1}' | head -n1)"
if [ -z "$EXPECTED" ]; then
  echo "No checksum found for ${ASSET}; refusing to install."; exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMP}/${ASSET}" | awk '{print $1}')"
else
  ACTUAL="$(shasum -a 256 "${TMP}/${ASSET}" | awk '{print $1}')"
fi
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "Checksum mismatch for ${ASSET}; refusing to install."; exit 1
fi

tar xzf "${TMP}/${ASSET}" -C "$TMP"
mkdir -p "$INSTALL_DIR"
mv "${TMP}/orcha" "$BIN_PATH"
chmod 755 "$BIN_PATH"

# Link into PATH (the real binary stays user-owned for safe self-update).
if [ -w "$LINK_DIR" ] || mkdir -p "$LINK_DIR" 2>/dev/null; then
  rm -f "$LINK_PATH"; ln -s "$BIN_PATH" "$LINK_PATH"
else
  echo "Linking ${LINK_PATH} (requires sudo)..."
  sudo mkdir -p "$LINK_DIR"; sudo rm -f "$LINK_PATH"; sudo ln -s "$BIN_PATH" "$LINK_PATH"
fi

echo "Installed orcha ${VERSION} -> ${BIN_PATH}"
echo "Laying down managed content..."
"$BIN_PATH" install
echo "Done. Run 'orcha doctor' to check dependencies (tmux, git, gh, claude)."
echo "Optional: 'orcha skills install' adds recommended agent skills (e.g. axi)."
