#!/bin/sh
# Run as root on Ubuntu 26.04. Copies overlay/ onto the root filesystem and starts
# the player. Never changes networking or reboots.
#   install.sh --binary PATH --env FILE --listen LAN_IP [--amp2] [--esound] [--upnp]
set -eu
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary= env_file= listen= amp2=
while [ "$#" -gt 0 ]; do
    case "$1" in
    --binary) binary=$2; shift 2 ;;
    --env) env_file=$2; shift 2 ;;
    --listen) listen=$2; shift 2 ;;
    --amp2) amp2=1; shift ;;
    --esound) esound=1; shift ;;
    --upnp) upnp=1; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
if [ "$(id -u)" != 0 ] || [ -z "$binary" ] || [ -z "$env_file" ] || [ -z "$listen" ]; then
    echo "usage: sudo $0 --binary PATH --env FILE --listen LAN_IP [--amp2] [--esound] [--upnp]" >&2
    exit 2
fi
test -x "$binary"
test -f "$env_file"
apt-get install -y --no-install-recommends mpv ca-certificates avahi-daemon avahi-utils

id slopify-player >/dev/null 2>&1 ||
    useradd --system --user-group --home-dir /var/lib/slopify-player --no-create-home --shell /usr/sbin/nologin slopify-player
usermod -a -G audio slopify-player

cp -r --no-preserve=ownership,mode "$here/overlay/." /
install -m 0600 "$env_file" /etc/default/slopify-player
if [ -e /usr/local/bin/slopify-player ]; then
    cp -p /usr/local/bin/slopify-player /usr/local/bin/slopify-player.previous
fi
install -m 0755 "$binary" /usr/local/bin/slopify-player.next
mv /usr/local/bin/slopify-player.next /usr/local/bin/slopify-player

flags="-local-listen=$listen:8080 -local-source-control -pulse-listen=$listen:4713"
if [ "${esound:-0}" = 1 ]; then
    flags="$flags -esound-listen=$listen:16001"
fi
if [ "${upnp:-0}" = 1 ]; then
    flags="$flags -upnp-listen=$listen:49494"
fi
if [ -n "$amp2" ]; then
    flags="$flags -volume-control=Digital -volume-card=sndrpihifiberry -volume-min=0 -volume-max=40 -volume-initial=20 -rotary-input=/dev/input/slopify-rotary -button-input=/dev/input/slopify-button"
fi
mkdir -p /etc/systemd/system/slopify-player.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/slopify-player %s\n' "$flags" > /etc/systemd/system/slopify-player.service.d/10-local.conf

udevadm control --reload-rules
udevadm trigger --subsystem-match=input
systemctl daemon-reload
systemctl enable slopify-pulse-advertise slopify-player
systemctl restart slopify-player slopify-pulse-advertise
systemctl is-active slopify-player
echo "Installed. Boot configuration and networking are separate; see player/deploy/README.md."
