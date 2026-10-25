#!/bin/sh
# Installs bin/sightline. herdr runs this as the [[build]] step when it installs or
# updates the plugin from GitHub, always in a fresh checkout.
#
# It downloads the prebuilt binary for this machine from the GitHub release that
# matches the version in herdr-plugin.toml and checks it against the release's
# checksums. If that fails and Go is installed, it builds from source instead.
# Version 0.0.0 means there's no release yet, so it goes straight to building.
#
# Plain POSIX sh, so it runs on a clean Mac or Linux box with nothing extra installed.
#
# Environment, mostly for testing:
#   SIGHTLINE_BUILD_FROM_SOURCE=1  skip the download and build with Go
#   SIGHTLINE_RELEASE_URL          where releases live, instead of GitHub
set -eu

repo="swmcc/herdr-sightline"
release_url="${SIGHTLINE_RELEASE_URL:-https://github.com/$repo/releases/download}"

cd "$(dirname "$0")/.."

say() { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }

version=$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' herdr-plugin.toml | head -n 1)

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) os="" ;;
esac

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) arch="" ;;
esac

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t sightline)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1"
  else
    warn "Neither curl nor wget is installed, so the binary can't be downloaded."
    return 1
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    warn "Neither sha256sum nor shasum is installed, so the download can't be checked."
    return 1
  fi
}

# Downloads, checks and installs the release binary. Returns non-zero without
# touching bin/ if anything goes wrong. These functions run as if conditions, where
# set -e doesn't apply, hence the explicit returns.
download() {
  archive="sightline_${version}_${os}_${arch}.tar.gz"
  base="$release_url/v$version"

  say "Downloading Sightline $version for $os/$arch"
  fetch "$base/$archive" "$tmp/$archive" || { warn "Couldn't download $base/$archive"; return 1; }
  fetch "$base/checksums.txt" "$tmp/checksums.txt" || { warn "Couldn't download $base/checksums.txt"; return 1; }

  want=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt")
  if [ -z "$want" ]; then
    warn "checksums.txt has no entry for $archive"
    return 1
  fi
  got=$(sha256 "$tmp/$archive") || return 1
  if [ "$got" != "$want" ]; then
    warn "Checksum mismatch for $archive: expected $want, got $got. Not installing it."
    return 1
  fi

  mkdir "$tmp/unpacked" || return 1
  tar -xzf "$tmp/$archive" -C "$tmp/unpacked" sightline || { warn "Couldn't unpack $archive"; return 1; }

  # Move into place in one step so a half written binary never ends up in bin/.
  mkdir -p bin || return 1
  mv -f "$tmp/unpacked/sightline" bin/sightline.new || return 1
  chmod 755 bin/sightline.new || return 1
  mv -f bin/sightline.new bin/sightline || return 1
  say "Installed bin/sightline $version (sha256 $got)"
}

build() {
  if ! command -v go >/dev/null 2>&1; then
    return 1
  fi
  if [ "$version" = "0.0.0" ]; then
    build_version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
  else
    build_version=$version
  fi
  say "Building Sightline $build_version from source with $(go version)"
  mkdir -p bin || return 1
  go build -trimpath -ldflags "-X main.version=$build_version" -o bin/sightline ./cmd/sightline || return 1
  say "Built bin/sightline"
}

# Only plain X.Y.Z versions have releases. Anything else, including 0.0.0 and an
# unreadable manifest, builds from source.
released=no
case "$version" in
  0.0.0 | "" | *[!0-9.]* | .* | *. | *..*) ;;
  *.*.*) released=yes ;;
esac
if [ "${SIGHTLINE_BUILD_FROM_SOURCE:-}" = "1" ]; then
  released=no
fi

if [ "$released" = yes ] && [ -n "$os" ] && [ -n "$arch" ]; then
  if download; then
    exit 0
  fi
  warn "Falling back to building from source."
fi

if build; then
  exit 0
fi

# Nothing worked. Say exactly what to do.
warn ""
if command -v go >/dev/null 2>&1; then
  warn "Building Sightline from source with $(go version) failed (the error is above)."
  warn "Sightline needs Go 1.24 or later. Update Go from https://go.dev/dl/ if it's older,"
  warn "or open an issue at https://github.com/$repo/issues with the output above."
elif [ "$released" = yes ] && [ -n "$os" ] && [ -n "$arch" ]; then
  warn "Sightline couldn't install the prebuilt binary for $os/$arch, and Go isn't"
  warn "installed to build it from source instead. To fix it, either:"
  warn ""
  warn "  1. Check you can reach https://github.com/$repo/releases/tag/v$version"
  warn "     (a proxy or firewall may be blocking the download), or"
  warn "  2. Install Go 1.24 or later from https://go.dev/dl/"
elif [ "$released" = yes ]; then
  warn "There's no prebuilt Sightline binary for $(uname -s)/$(uname -m), so it has to"
  warn "be built from source, and Go isn't installed. To fix it:"
  warn ""
  warn "  Install Go 1.24 or later from https://go.dev/dl/"
else
  warn "This version of Sightline ($version) has no prebuilt binary, so it has to be"
  warn "built from source, and Go isn't installed. To fix it, either:"
  warn ""
  warn "  1. Install a released version: herdr plugin install $repo --ref vX.Y.Z"
  warn "     (see https://github.com/$repo/releases), or"
  warn "  2. Install Go 1.24 or later from https://go.dev/dl/"
fi
warn ""
warn "Then install it again: herdr plugin install $repo"
exit 1
