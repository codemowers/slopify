# ath79 routers have no FPU. Vorbis is excluded by mips.softfloat automatically.
. "$SLOPIFY_OPENWRT/profiles/common.sh"
SLOPIFY_GOARCH=mips
SLOPIFY_GOMIPS=softfloat
SLOPIFY_PROFILE_PACKAGES="kmod-usb2 kmod-usb-audio"
slopify_prepare() {
 mkdir -p slopify-files/etc/sysctl.d
 cat > slopify-files/etc/sysctl.d/90-slopify-client.conf <<-SYSCTL
net.ipv4.ip_forward=0
net.ipv6.conf.all.forwarding=0
SYSCTL
 cp "$SLOPIFY_OPENWRT/profiles/files/router-network" slopify-files/etc/uci-defaults/90-slopify-network
 # Leave enough of the 128 MiB RAM for OpenWrt and the Wi-Fi drivers.
 cat > slopify-files/etc/uci-defaults/89-slopify-router-player <<'CONFIG'
uci -q get slopify.player.read_ahead_mib >/dev/null || uci set slopify.player.read_ahead_mib='8'
uci -q get slopify.player.memory_limit >/dev/null || uci set slopify.player.memory_limit='32MiB'
uci commit slopify
exit 0
CONFIG
}
