# Wired DHCP client only: no DHCP/RA server, PPP, Wi-Fi supplicant or firewall. OpenSSH replaces
# Dropbear. LuCI without luci-light, which pulls in the firewall.
SLOPIFY_COMMON_PACKAGES="kmod-usb-audio avahi-dbus-daemon alsa-utils avahi-utils luci-mod-admin-full luci-proto-ipv6 luci-theme-bootstrap rpcd-mod-rrdns uhttpd uhttpd-mod-ubus openssh-server -dropbear -firewall4 -nftables -kmod-nft-offload -dnsmasq -odhcpd-ipv6only -ppp -ppp-mod-pppoe -wpad-basic-mbedtls"
slopify_prepare() { :; }
