# OpenWrt's /etc/profile displays /etc/banner; add live player information.
[ ! -t 1 ] || /usr/libexec/slopify/motd
