#!/bin/sh
# Build OpenWrt speaker images: compile the static player here, then add it to
# the official Image Builder's output with official packages only.
# Usage: build.sh [profile...] (default: rpi-3 cubietruck)
# Optional environment: SLOPIFY_SERVER, SSH_AUTHORIZED_KEYS (key text),
# EXTRA_PACKAGES, OUT (default dist/openwrt).
set -eu
repo="$(cd "$(dirname "$0")/../.." && pwd)"
out="${OUT:-$repo/dist/openwrt}"
[ $# -gt 0 ] || set -- rpi-3 cubietruck
for profile in "$@"; do
	SLOPIFY_OPENWRT="$repo/player/openwrt"
	. "$SLOPIFY_OPENWRT/profiles/$profile.sh"
	bin="$out/.player-$profile"
	mkdir -p "$out/$profile" "$bin"
	(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$SLOPIFY_GOARCH" GOARM="${SLOPIFY_GOARM:-}" \
		go build -trimpath -ldflags='-s -w' -o "$bin/slopify-player" ./player)
	# The container runs as its own unprivileged user.
	chmod a+rwx "$out/$profile"
	docker run --rm -v "$repo/player/openwrt:/slopify:ro" -v "$bin:/player:ro" -v "$out/$profile:/out" \
		-e PROFILE_NAME="$profile" -e SLOPIFY_SERVER="${SLOPIFY_SERVER:-}" \
		-e SSH_AUTHORIZED_KEYS="${SSH_AUTHORIZED_KEYS:-}" -e EXTRA_PACKAGES="${EXTRA_PACKAGES:-}" \
		"openwrt/imagebuilder:$SLOPIFY_IMAGEBUILDER" sh /slopify/image-build.sh
	rm -rf "$bin"
done
