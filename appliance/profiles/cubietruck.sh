# Cubietruck (sunxi/cortexa7, Allwinner A20). Gigabit Ethernet is built into the
# kernel. Audio includes the onboard analog codec, S/PDIF and USB DACs.
. "$SLOPIFY_OPENWRT/profiles/common.sh"
SLOPIFY_IMAGEBUILDER=sunxi-cortexa7-25.12.6
SLOPIFY_DEVICE=cubietech_cubietruck
SLOPIFY_GOARCH=arm
SLOPIFY_GOARM=7
SLOPIFY_PROFILE_PACKAGES="kmod-sound-soc-sunxi kmod-sound-soc-sunxi-spdif kmod-brcmfmac cypress-firmware-43362-sdio"
