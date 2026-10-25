#!/bin/sh
# Tests scripts/install.sh against a fake release served from disk with file:// URLs.
# Each case runs in a fresh copy of the repo, like herdr's fresh checkout.
# Run it with: sh scripts/install_test.sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t sightline-test)
trap 'rm -rf "$tmp"' EXIT

command -v curl >/dev/null 2>&1 || { echo "install_test.sh needs curl"; exit 1; }

case "$(uname -s)" in Darwin) os=darwin ;; *) os=linux ;; esac
case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
archive="sightline_9.9.9_${os}_${arch}.tar.gz"

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# A PATH with the tools the install script needs but without Go or git.
nogo="$tmp/nogo-bin"
mkdir "$nogo"
for tool in sh sed head uname mktemp rm curl awk sha256sum shasum perl mkdir tar gzip mv chmod dirname cat; do
  if p=$(command -v "$tool" 2>/dev/null); then ln -s "$p" "$nogo/$tool"; fi
done
withgo="$nogo"
if gobin=$(command -v go 2>/dev/null); then withgo="$nogo:$(dirname "$gobin")"; fi

# The fake release: a stand in binary, its archive and a checksums file.
release="$tmp/release/v9.9.9"
mkdir -p "$release" "$tmp/payload"
printf '#!/bin/sh\necho "sightline 9.9.9 (fake)"\n' >"$tmp/payload/sightline"
chmod 755 "$tmp/payload/sightline"
tar -czf "$release/$archive" -C "$tmp/payload" sightline
printf '%s  %s\n' "$(sha256 "$release/$archive")" "$archive" >"$release/checksums.txt"

bad="$tmp/bad-release/v9.9.9"
mkdir -p "$bad"
cp "$release/$archive" "$bad/$archive"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$archive" >"$bad/checksums.txt"

failures=0
n=0

# run <name> <manifest version> <release dir> <PATH> <want exit> <want binary: fake|source|none> <want output>
run() {
  name=$1 version=$2 rel=$3 path=$4 want_exit=$5 want_bin=$6 want_out=$7
  n=$((n + 1))
  work="$tmp/case$n"
  mkdir "$work"
  (cd "$root" && git ls-files --cached --others --exclude-standard | tar -cf - -T -) | tar -xf - -C "$work"
  sed "s|^version = \".*\"|version = \"$version\"|" "$root/herdr-plugin.toml" >"$work/herdr-plugin.toml"

  set +e
  out=$(cd "$work" && env PATH="$path" SIGHTLINE_RELEASE_URL="file://$rel" sh scripts/install.sh 2>&1)
  code=$?
  set -e

  problem=""
  if [ "$code" -ne "$want_exit" ]; then problem="exit $code, want $want_exit"; fi
  case "$want_bin" in
    none) [ ! -e "$work/bin/sightline" ] || problem="$problem; bin/sightline exists" ;;
    fake) cmp -s "$work/bin/sightline" "$tmp/payload/sightline" || problem="$problem; bin/sightline isn't the released binary" ;;
    source) "$work/bin/sightline" version >/dev/null 2>&1 && ! cmp -s "$work/bin/sightline" "$tmp/payload/sightline" || problem="$problem; bin/sightline wasn't built from source" ;;
  esac
  case "$out" in *"$want_out"*) ;; *) problem="$problem; output lacks \"$want_out\"" ;; esac

  if [ -n "$problem" ]; then
    failures=$((failures + 1))
    printf 'FAIL %s: %s\n%s\n\n' "$name" "$problem" "$out"
  else
    printf 'ok   %s\n' "$name"
  fi
}

run "downloads and verifies the release" 9.9.9 "$tmp/release" "$nogo" 0 fake "Installed bin/sightline 9.9.9"
run "refuses a checksum mismatch without Go" 9.9.9 "$tmp/bad-release" "$nogo" 1 none "Checksum mismatch"
run "says what to do when the release is missing" 9.9.9 "$tmp/missing" "$nogo" 1 none "Install Go 1.24 or later"
run "0.0.0 builds from source and needs Go" 0.0.0 "$tmp/release" "$nogo" 1 none "has no prebuilt binary"
run "odd versions never hit the network" '1.0.0-rc1/../x' "$tmp/release" "$nogo" 1 none "has no prebuilt binary"

if [ "$withgo" != "$nogo" ]; then
  run "checksum mismatch falls back to Go" 9.9.9 "$tmp/bad-release" "$withgo" 0 source "Built bin/sightline"
  run "0.0.0 builds from source with Go" 0.0.0 "$tmp/release" "$withgo" 0 source "Built bin/sightline"
else
  echo "skip the build from source cases: Go isn't installed"
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures of $n install script cases failed"
  exit 1
fi
echo "All $n install script cases passed."
