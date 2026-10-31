# Raspberry Pi 3 (bcm27xx/bcm2710). Ethernet and the USB host are built into
# the kernel. Every I2S sound board driver is installed, so changing board only
# takes a dtoverlay line in /boot/config.txt.
. "$SLOPIFY_OPENWRT/profiles/common.sh"
SLOPIFY_IMAGEBUILDER=bcm27xx-bcm2710-25.12.6
SLOPIFY_DEVICE=rpi-3
SLOPIFY_GOARCH=arm64
SLOPIFY_SOUND_SOC="ac97 adau1977-adc allo-boss2-dac allo-boss-dac allo-digione allo-katana-codec allo-piano-dac allo-piano-dac-plus audioinjector-isolated-soundcard audioinjector-octo-soundcard audioinjector-pi-soundcard audiosense-pi bcm2835-i2s chipdip-dac dacberry-soundcard digidac1-soundcard dionaudio-loco dionaudio-loco-v2 dmic fe-pi googlevoicehat hifiberry-adc hifiberry-amp hifiberry-dac hifiberry-dacplus hifiberry-dacplusadc hifiberry-dacplusadc-pro hifiberry-dacplusdsp hifiberry-dacplushd hifiberry-digi iqaudio-codec iqaudio-dac iqaudio-digi i-sabe-q2m justboom-both justboom-dac justboom-digi merus-amp pifi-40-amp pisound rpi-cirrus rpi-dac rpi-proto rpi-simple-soundcard rpi-wm8804-soundcard spdif wm8960"
SLOPIFY_PROFILE_PACKAGES="$(for soc in $SLOPIFY_SOUND_SOC; do printf 'kmod-sound-soc-%s ' "$soc"; done)kmod-input-evdev kmod-input-gpio-encoder kmod-input-gpio-keys -kmod-brcmfmac -cypress-firmware-43430-sdio -brcmfmac-nvram-43430-sdio -cypress-firmware-43455-sdio -brcmfmac-nvram-43455-sdio"
slopify_prepare() {
	cat "$SLOPIFY_OPENWRT/profiles/rpi-3-config.txt" >> target/linux/bcm27xx/image/config.txt
	# kmodloader ignores the firmware's snd_bcm2835.enable_hdmi on the command line.
	mkdir -p slopify-files/etc/modules.d
	echo 'snd-bcm2835 enable_hdmi=1' > slopify-files/etc/modules.d/68-sound-arm-bcm2835
}
