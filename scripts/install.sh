#!/bin/sh
# Installs the qdrive binary from a GitHub release (Linux amd64/arm64 only).
#
#   curl -fsSL https://raw.githubusercontent.com/zzfn/QDrive/main/scripts/install.sh | sh
#
# QDRIVE_VERSION      a release tag such as v0.1.0 (default: the latest release)
# QDRIVE_INSTALL_DIR  install directory (default: ~/.local/bin)
#
# The script never edits shell profiles and never uses sudo. It fails if no
# release (or no asset for your architecture) has been published yet.
set -eu

# Parse the complete script before performing any installation when piped to sh.
main() {
repository="zzfn/QDrive"
version="${QDRIVE_VERSION:-latest}"
bin="${QDRIVE_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}"

fail() {
  echo "qdrive: $*" >&2
  exit 1
}

os="$(uname -s)"
[ "$os" = "Linux" ] || fail "unsupported system $os; only Linux is supported"

case "$(uname -m)" in
  x86_64 | amd64) arch="amd64" ;;
  aarch64 | arm64) arch="arm64" ;;
  *) fail "unsupported architecture $(uname -m); only amd64/x86_64 and arm64/aarch64 are supported" ;;
esac

command -v curl > /dev/null 2>&1 || fail "curl is required"
command -v sha256sum > /dev/null 2>&1 || fail "sha256sum is required"
command -v tar > /dev/null 2>&1 || fail "tar is required"

asset="qdrive-linux-$arch.tar.gz"
if [ "$version" = "latest" ]; then
  url="https://github.com/$repository/releases/latest/download/$asset"
else
  url="https://github.com/$repository/releases/download/$version/$asset"
fi

work="$(mktemp -d)"
tmp=""
trap '[ -z "$tmp" ] || rm -f "$tmp"; rm -rf "$work"' EXIT
trap 'exit 1' HUP INT TERM

echo "Downloading $url"
curl -fsSL "$url" -o "$work/$asset" ||
  fail "download failed: $url (release $version may not be published yet; see https://github.com/$repository/releases)"
curl -fsSL "$url.sha256" -o "$work/$asset.sha256" ||
  fail "checksum download failed: $url.sha256 (release $version may not be published yet)"

# Require exactly one SHA-256 entry for the downloaded archive.
[ "$(wc -l < "$work/$asset.sha256")" -eq 1 ] || fail "invalid checksum file for $asset"
read -r digest filename < "$work/$asset.sha256" || fail "invalid checksum file for $asset"
[ "${#digest}" -eq 64 ] && [ "$filename" = "$asset" ] || fail "invalid checksum file for $asset"
case "$digest" in *[!0-9a-fA-F]*) fail "invalid checksum file for $asset" ;; esac
(cd "$work" && sha256sum -c "$asset.sha256" > /dev/null 2>&1) ||
  fail "checksum verification failed for $asset; nothing was installed"

mkdir "$work/extract"
tar -xzf "$work/$asset" -C "$work/extract" qdrive || fail "failed to extract $asset"
[ -f "$work/extract/qdrive" ] && [ ! -L "$work/extract/qdrive" ] || fail "archive $asset does not contain a regular qdrive file"

mkdir -p "$bin" || fail "cannot create $bin (no sudo is used; set QDRIVE_INSTALL_DIR to a writable directory)"
[ ! -d "$bin/qdrive" ] || fail "$bin/qdrive is a directory"
# Stage next to the target, then rename, so a failure never leaves a broken binary.
tmp="$(mktemp "$bin/.qdrive.XXXXXXXX")" || fail "cannot create a temporary file in $bin"
if ! cp "$work/extract/qdrive" "$tmp" || ! chmod 755 "$tmp" || ! mv -f "$tmp" "$bin/qdrive"; then
  fail "cannot write $bin/qdrive (no sudo is used; set QDRIVE_INSTALL_DIR to a writable directory)"
fi
tmp=""

echo "Installed qdrive ($version) to $bin/qdrive"
case ":${PATH:-}:" in
  *":$bin:"*) ;;
  *) echo "Note: $bin is not in your PATH. Add it yourself, e.g.: export PATH=\"$bin:\$PATH\"" ;;
esac
}

main "$@"
