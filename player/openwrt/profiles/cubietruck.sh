# Cubietruck (sunxi/cortexa7, Allwinner A20). Gigabit Ethernet is built into the
# kernel. The onboard analog codec and USB DACs are the audio outputs.
. "$SLOPIFY_OPENWRT/profiles/common.sh"
SLOPIFY_IMAGEBUILDER=sunxi-cortexa7-25.12.6
SLOPIFY_DEVICE=cubietech_cubietruck
SLOPIFY_GOARCH=arm
SLOPIFY_GOARM=7
SLOPIFY_PROFILE_PACKAGES="kmod-sound-soc-sunxi -kmod-brcmfmac"
