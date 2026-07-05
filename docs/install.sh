#!/bin/sh
# ttorch installer (macOS / Linux / WSL).
#   curl -fsSL https://raw.githubusercontent.com/nution101/ttorch/main/docs/install.sh | sh
set -eu

REPO="${TTORCH_REPO:-nution101/ttorch}"
INSTALL_DIR="${TTORCH_HOME:-$HOME/.ttorch}/bin"
BIN_PATH="$INSTALL_DIR/ttorch"

# Choose a PATH dir we can link into.
if [ -n "${TTORCH_BIN_DIR:-}" ]; then
  LINK_DIR="$TTORCH_BIN_DIR"
else
  case ":$PATH:" in
    *":$HOME/.local/bin:"*) LINK_DIR="$HOME/.local/bin" ;;
    *) LINK_DIR="/usr/local/bin" ;;
  esac
fi
LINK_PATH="$LINK_DIR/ttorch"

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

ASSET="ttorch-${VERSION}-${OS}-${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "Downloading ttorch ${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}"
curl -fsSL "${BASE}/checksums.txt" -o "${TMP}/checksums.txt"

# Verify the provenance of checksums.txt with cosign (Sigstore keyless) before
# trusting any checksum in it. The release workflow signs checksums.txt and
# attaches checksums.txt.cosign.bundle alongside it.
#
# Strict by default: when cosign is installed, a missing OR invalid signature is
# fatal. We never silently downgrade to a sha256-only check — an attacker who can
# tamper with the download could simply strip the bundle, and sha256 alone proves
# only that the download is internally consistent, not that it came from this repo.
# Every ttorch release from v0.1.0 on is signed, so strict mode never blocks a real
# release.
#
# Opt out with TTORCH_INSTALL_ALLOW_UNSIGNED=1 for the rare case that must proceed
# without a verifiable signature (an air-gapped mirror, or a genuinely unsigned
# release): a MISSING bundle then degrades to a loud warning + sha256 fallback. A
# bundle that is present but FAILS verification is always fatal regardless — that is
# an active tampering signal, not a missing-signature one. When cosign is absent we
# cannot verify provenance at all, so we fall back to sha256 with a clear warning.
if command -v cosign >/dev/null 2>&1; then
  if curl -fsSL "${BASE}/checksums.txt.cosign.bundle" -o "${TMP}/checksums.txt.cosign.bundle" 2>/dev/null; then
    echo "Verifying checksums signature with cosign (keyless)..."
    if ! cosign verify-blob \
      --bundle "${TMP}/checksums.txt.cosign.bundle" \
      --certificate-identity-regexp "^https://github.com/${REPO}/\.github/workflows/.+" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      "${TMP}/checksums.txt"; then
      echo "cosign signature verification FAILED for ${VERSION}; refusing to install."; exit 1
    fi
  elif [ "${TTORCH_INSTALL_ALLOW_UNSIGNED:-}" = "1" ]; then
    echo "WARNING: cosign is installed but no signature bundle was found for ${VERSION}, and"
    echo "         TTORCH_INSTALL_ALLOW_UNSIGNED=1 is set. Provenance could NOT be verified"
    echo "         (the release may predate signing, or the bundle may have been stripped)."
    echo "         Falling back to the sha256 check only."
  else
    echo "ERROR: cosign is installed but no signature bundle was found for ${VERSION}."
    echo "       Refusing to install: a missing signature can mean the download was tampered"
    echo "       with in transit (the bundle stripped to force a weaker check). Every ttorch"
    echo "       release from v0.1.0 on is signed. To proceed anyway (air-gapped mirror or a"
    echo "       genuinely unsigned release), re-run with TTORCH_INSTALL_ALLOW_UNSIGNED=1."
    exit 1
  fi
else
  echo "Warning: cosign not installed; skipping signature verification."
  echo "         The sha256 check below confirms the download is internally consistent but"
  echo "         does NOT prove it came from this repo. Install cosign for that guarantee:"
  echo "         https://github.com/sigstore/cosign"
fi

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
mv "${TMP}/ttorch" "$BIN_PATH"
chmod 755 "$BIN_PATH"

# Link into PATH (the real binary stays user-owned for safe self-update).
if [ -w "$LINK_DIR" ] || mkdir -p "$LINK_DIR" 2>/dev/null; then
  rm -f "$LINK_PATH"; ln -s "$BIN_PATH" "$LINK_PATH"
else
  echo "Linking ${LINK_PATH} (requires sudo)..."
  sudo mkdir -p "$LINK_DIR"; sudo rm -f "$LINK_PATH"; sudo ln -s "$BIN_PATH" "$LINK_PATH"
fi

echo "Installed ttorch ${VERSION} -> ${BIN_PATH}"
echo "Laying down managed content..."
"$BIN_PATH" install
echo "Done. Run 'ttorch doctor' to check dependencies (tmux, git, gh, claude)."
echo "ttorch installs recommended agent skills (e.g. axi, ponytail) automatically before a team launches; 'ttorch skills install' forces it now (needs npx/Node)."
