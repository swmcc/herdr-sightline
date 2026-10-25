#!/bin/sh
# Installs bin/sightline when herdr installs the plugin from GitHub.
# For now this builds from source, so Go is needed. Prebuilt binaries replace this in #29.
set -eu

if ! command -v go >/dev/null 2>&1; then
  echo "Sightline needs Go to build until prebuilt binaries ship (see #29)." >&2
  echo "Install Go from https://go.dev/dl/ and run: herdr plugin update swmcc.sightline" >&2
  exit 1
fi

mkdir -p bin
go build -trimpath -o bin/sightline ./cmd/sightline
echo "Built bin/sightline"
