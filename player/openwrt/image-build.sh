#!/bin/sh
# Runs inside openwrt/imagebuilder.
set -eu
cd /builder
SLOPIFY_OPENWRT=/slopify
. "/slopify/profiles/$PROFILE_NAME.sh"
cp -rp /slopify/files slopify-files
mkdir -p slopify-files/usr/bin slopify-files/etc/uci-defaults
install -m 0755 /player/slopify-player slopify-files/usr/bin/slopify-player
# These modes replace those of /, /etc, /lib and /usr, whatever the checkout's umask.
chmod -R u=rwX,go=rX slopify-files
slopify_prepare
if [ -n "$SLOPIFY_SERVER" ]; then
	case "$SLOPIFY_SERVER" in https://*|http://*) ;; *) echo "SLOPIFY_SERVER must be an http(s) origin" >&2; exit 1 ;; esac
	case "$SLOPIFY_SERVER" in *[\'\\\ ]*) echo "SLOPIFY_SERVER contains unsupported characters" >&2; exit 1 ;; esac
	printf "uci set slopify.player.server='%s'\nuci commit slopify\nexit 0\n" "$SLOPIFY_SERVER" > slopify-files/etc/uci-defaults/95-slopify-server
fi
if [ -n "$SSH_AUTHORIZED_KEYS" ]; then
	mkdir -p slopify-files/root/.ssh slopify-files/etc/ssh/sshd_config.d
	printf '%s\n' "$SSH_AUTHORIZED_KEYS" > slopify-files/root/.ssh/authorized_keys
	chmod 0700 slopify-files/root/.ssh
	chmod 0600 slopify-files/root/.ssh/authorized_keys
	printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' > slopify-files/etc/ssh/sshd_config.d/slopify.conf
fi
make image PROFILE="$SLOPIFY_DEVICE" FILES=/builder/slopify-files EXTRA_IMAGE_NAME=slopify \
	PACKAGES="$SLOPIFY_COMMON_PACKAGES $SLOPIFY_PROFILE_PACKAGES $EXTRA_PACKAGES"
find bin/targets -type f \( -name '*.img.gz' -o -name '*.manifest' -o -name sha256sums \) -exec cp {} /out/ \;
ls -l /out
