#!/bin/sh
# Runs inside openwrt/imagebuilder.
set -eu
cd /builder
SLOPIFY_OPENWRT=/slopify
. "/slopify/profiles/$PROFILE_NAME.sh"
cp -rp /slopify/files slopify-files
mkdir -p slopify-files/usr/bin slopify-files/etc/uci-defaults
install -m 0755 /player/slopify-player slopify-files/usr/bin/slopify-player
install -m 0755 /player/slopify-ir slopify-files/usr/bin/slopify-ir
# These modes replace those of /, /etc, /lib and /usr, whatever the checkout's umask.
chmod -R u=rwX,go=rX slopify-files
slopify_prepare
if [ -n "$SLOPIFY_SERVER" ]; then
	case "$SLOPIFY_SERVER" in https://*|http://*) ;; *) echo "SLOPIFY_SERVER must be an http(s) origin" >&2; exit 1 ;; esac
	case "$SLOPIFY_SERVER" in *[\'\\\ ]*) echo "SLOPIFY_SERVER contains unsupported characters" >&2; exit 1 ;; esac
	printf "uci set slopify.player.server='%s'\nuci commit slopify\nexit 0\n" "$SLOPIFY_SERVER" > slopify-files/etc/uci-defaults/95-slopify-server
fi
if [ -n "$SSH_AUTHORIZED_KEYS" ]; then
	mkdir -p slopify-files/etc/dropbear
	printf '%s\n' "$SSH_AUTHORIZED_KEYS" > slopify-files/etc/dropbear/authorized_keys
	chmod 0700 slopify-files/etc/dropbear
	chmod 0600 slopify-files/etc/dropbear/authorized_keys
	# Keys only: no password login over SSH, even once a root password is set.
	printf "uci set dropbear.@dropbear[0].PasswordAuth='off'\nuci set dropbear.@dropbear[0].RootPasswordAuth='off'\nuci commit dropbear\nexit 0\n" \
		> slopify-files/etc/uci-defaults/95-slopify-ssh-keys
fi
make image PROFILE="$SLOPIFY_DEVICE" FILES=/builder/slopify-files EXTRA_IMAGE_NAME=slopify \
	PACKAGES="$SLOPIFY_COMMON_PACKAGES $SLOPIFY_PROFILE_PACKAGES $EXTRA_PACKAGES"
find bin/targets -type f \( -name '*.img.gz' -o -name '*sysupgrade.bin' -o -name '*kernel.bin' -o -name '*.manifest' -o -name '*.bom.cdx.json' -o -name profiles.json -o -name sha256sums \) -exec cp {} /out/ \;
ls -l /out
