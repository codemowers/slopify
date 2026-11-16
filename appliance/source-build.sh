#!/bin/sh
# Experimental source-built non-RT IR images for Pi 3 or Cubietruck.
# Usage: source-build.sh [rpi-3|cubietruck] (default: rpi-3)
# SOURCE_WORKDIR and OUT must be on disk, not /tmp or /dev/shm.
set -eu
umask 022
repo="$(cd "$(dirname "$0")/.." && pwd)"
[ $# -le 1 ] || { echo 'Use one profile per source checkout' >&2; exit 1; }
profile="${1:-rpi-3}"
case "$profile" in
 rpi-3) work_name=rpi3 ;;
 cubietruck) work_name=cubietruck ;;
 *) echo "Unsupported source profile: $profile" >&2; exit 1 ;;
esac
SLOPIFY_OPENWRT="$repo/appliance"
. "$SLOPIFY_OPENWRT/profiles/$profile.sh"
work="${SOURCE_WORKDIR:-$repo/.build-tmp/openwrt-$work_name}"
out="${OUT:-$repo/dist/openwrt-ir}"
revision=64900e756efa667774214f9c6b17c113222e01f7
image=slopify-openwrt-source:bookworm
set -- "$profile"
case "$work" in /tmp|/tmp/*|/dev/shm|/dev/shm/*) echo 'Source builds require disk-backed scratch space' >&2; exit 1 ;; esac
case "$out" in /tmp|/tmp/*|/dev/shm|/dev/shm/*) echo 'Output requires disk-backed storage' >&2; exit 1 ;; esac
mkdir -p "$work" "$out" "$work/player" "$work/go-tmp"
work="$(cd "$work" && pwd -P)"
out="$(cd "$out" && pwd -P)"
case "$work:$out" in /tmp:*|/tmp/*:*|/dev/shm:*|/dev/shm/*:*|*:/tmp|*:/tmp/*|*:/dev/shm|*:/dev/shm/*) echo 'Build paths resolve to memory-backed scratch space' >&2; exit 1 ;; esac
if [ ! -d "$work/source/.git" ]; then
 git clone --depth 1 --branch v25.12.6 https://git.openwrt.org/openwrt/openwrt.git "$work/source"
fi
[ "$(git -C "$work/source" rev-parse HEAD)" = "$revision" ] || { echo 'Unexpected OpenWrt revision; use a separate work directory' >&2; exit 1; }
if ! docker image inspect "$image" >/dev/null 2>&1; then
 docker build -f "$repo/appliance/source.Dockerfile" -t "$image" "$repo"
fi
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$SLOPIFY_GOARCH" GOARM="${SLOPIFY_GOARM:-}" GOTMPDIR="$work/go-tmp" \
 go build -tags "${PLAYER_BUILD_TAGS:-}" -trimpath -ldflags='-s -w' -o "$work/player/slopify-player" ./player
 CGO_ENABLED=0 GOOS=linux GOARCH="$SLOPIFY_GOARCH" GOARM="${SLOPIFY_GOARM:-}" GOTMPDIR="$work/go-tmp" go build -trimpath -ldflags='-s -w' -o "$work/player/slopify-ir" ./player/tools/ir)
for profile in "$@"; do
 # Leave a reserve for the running app; never prune caches or old build outputs.
 available=$(df -Pk "$work" | awk 'NR==2 {print $4}')
 [ "$available" -gt 8388608 ] || { echo 'Source build needs at least 8 GiB free before the next target' >&2; exit 1; }
 docker run --rm --name "slopify-openwrt-source-$profile" \
  --user "$(id -u):$(id -g)" \
  -v "$work/source:/build" -v "$repo/appliance:/slopify:ro" \
  -v "$work/player:/player:ro" -v "$out:/out" \
  -e HOME=/build -e PROFILE_NAME="$profile" -e JOBS="${JOBS:-8}" \
  -e SLOPIFY_SERVER="${SLOPIFY_SERVER:-}" -e SSH_AUTHORIZED_KEYS="${SSH_AUTHORIZED_KEYS:-}" \
  -e EXTRA_PACKAGES="${EXTRA_PACKAGES:-}" \
  "$image" sh -c 'snapshot="/build/.slopify-build-$PROFILE_NAME.sh"; cp /slopify/source/build-in-container.sh "$snapshot"; exec sh "$snapshot"'
done
