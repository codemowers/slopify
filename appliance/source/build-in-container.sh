#!/bin/sh
# Runs as an unprivileged user in source.Dockerfile's container.
set -eu
umask 022
cd /build
SLOPIFY_OPENWRT=/slopify
export SLOPIFY_OPENWRT
. "/slopify/profiles/$PROFILE_NAME.sh"
case "$PROFILE_NAME" in
 rpi-3)
  board=bcm27xx; subtarget=bcm2710; elf_machine=183
  board_configs="target/linux/bcm27xx/config-6.12 target/linux/bcm27xx/bcm2710/config-6.12 target/linux/bcm27xx/image/config.txt"
  receiver_module=gpio-ir-recv; receiver_setting=CONFIG_IR_GPIO_CIR=m
  ;;
 cubietruck)
  board=sunxi; subtarget=cortexa7; elf_machine=40
  board_configs=target/linux/sunxi/config-6.12
  receiver_module=sunxi-cir; receiver_setting=CONFIG_IR_SUNXI=m
  ;;
 *) echo "Unsupported source profile: $PROFILE_NAME" >&2; exit 1 ;;
esac
if [ -f .config ] && ! grep -qx "CONFIG_TARGET_${board}_${subtarget}=y" .config; then
 echo 'Existing checkout targets a different board; use a separate source work directory' >&2
 exit 1
fi
if [ -f .slopify-source-profile ]; then
 [ "$(cat .slopify-source-profile)" = "$PROFILE_NAME" ] || { echo 'Use a separate source checkout for each profile' >&2; exit 1; }
fi
printf '%s\n' "$PROFILE_NAME" > .slopify-source-profile
# Restore pinned board defaults; keep upstream CPU, toolchain and scheduling.
for config in $board_configs; do
 git show "HEAD:$config" > "$config"
done
mkdir -p package/slopify
cp -R /slopify/source/packages/slopify-ir package/slopify/

# Use the release-pinned feeds. Download once; later targets reuse them.
if [ ! -f .slopify-feeds-ready ]; then
 ./scripts/feeds update packages luci
 ./scripts/feeds install -a -p packages
 ./scripts/feeds install -a -p luci
 touch .slopify-feeds-ready
fi

# The stock v4l-utils package disables the upstream BPF IR decoders.
feed_patch=/slopify/source/patches/v4l-utils-bpf.patch
if ! git -C feeds/packages apply --reverse --check "$feed_patch" 2>/dev/null; then
 git -C feeds/packages apply --check "$feed_patch"
 git -C feeds/packages apply "$feed_patch"
fi

# Correct source permissions inherited from a restrictive host checkout umask.
sh /slopify/source/normalize-checkout.sh /build /build/feeds/packages /build/feeds/luci

# Keep the official profile's packages, with the local IR module bundle and tools.
cat > .config <<CONFIG
CONFIG_TARGET_${board}=y
CONFIG_TARGET_${board}_${subtarget}=y
CONFIG_TARGET_${board}_${subtarget}_DEVICE_${SLOPIFY_DEVICE}=y
CONFIG_TARGET_ROOTFS_SQUASHFS=y
# CONFIG_TARGET_ROOTFS_EXT4FS is not set
# CONFIG_TARGET_ROOTFS_TARGZ is not set
CONFIG_TARGET_KERNEL_PARTSIZE=64
CONFIG_TARGET_ROOTFS_PARTSIZE=256
# CONFIG_KERNEL_DEBUG_INFO is not set
CONFIG_PACKAGE_kmod-slopify-ir=y
CONFIG_PACKAGE_v4l-utils=y
CONFIG_PACKAGE_kmod-input-evdev=y
CONFIG_SIGNED_PACKAGES=y
CONFIG
for package in $SLOPIFY_COMMON_PACKAGES $SLOPIFY_PROFILE_PACKAGES ${EXTRA_PACKAGES:-}; do
 case "$package" in
  -*) printf '# CONFIG_PACKAGE_%s is not set\n' "${package#-}" ;;
  *) printf 'CONFIG_PACKAGE_%s=y\n' "$package" ;;
 esac >> .config
done
# OpenWrt tracks this board Makefile, not changes to each target.mk.
# Its normal defconfig path supplies the scanner's required make context.
touch "target/linux/$board/Makefile"
make defconfig
for package in kmod-slopify-ir v4l-utils; do
 grep -qx "CONFIG_PACKAGE_$package=y" .config || { echo "Missing requested package: $package" >&2; exit 1; }
done

# Build the same overlay as Image Builder without modifying host media or devices.
mkdir -p slopify-files
cp -R /slopify/files/. slopify-files/
mkdir -p slopify-files/usr/bin
install -m 0755 /player/slopify-player slopify-files/usr/bin/slopify-player
install -m 0755 /player/slopify-ir slopify-files/usr/bin/slopify-ir
chmod -R u=rwX,go=rX slopify-files
slopify_prepare
# Full source builds use TOPDIR/files. FILES= is an Image Builder interface;
# passing it on make's command line would override every kmod's FILES list.
if [ ! -e files ]; then
 ln -s slopify-files files
elif [ "$(readlink files)" != slopify-files ]; then
 echo 'This source tree already has a different files overlay; use a separate work directory' >&2
 exit 1
fi
if [ -n "${SLOPIFY_SERVER:-}" ]; then
 case "$SLOPIFY_SERVER" in http://*|https://*) ;; *) echo 'SLOPIFY_SERVER must be an HTTP(S) origin' >&2; exit 1 ;; esac
 case "$SLOPIFY_SERVER" in *[\'\\\ ]*) echo 'Unsupported characters in SLOPIFY_SERVER' >&2; exit 1 ;; esac
 printf "uci set slopify.player.server='%s'\nuci commit slopify\nexit 0\n" "$SLOPIFY_SERVER" > slopify-files/etc/uci-defaults/95-slopify-server
fi
if [ -n "${SSH_AUTHORIZED_KEYS:-}" ]; then
 mkdir -p slopify-files/etc/dropbear
 printf '%s\n' "$SSH_AUTHORIZED_KEYS" > slopify-files/etc/dropbear/authorized_keys
 chmod 0700 slopify-files/etc/dropbear
 chmod 0600 slopify-files/etc/dropbear/authorized_keys
 printf "uci set dropbear.@dropbear[0].PasswordAuth='off'\nuci set dropbear.@dropbear[0].RootPasswordAuth='off'\nuci commit dropbear\nexit 0\n" > slopify-files/etc/uci-defaults/95-slopify-ssh-keys
fi
# Leave GPIO opt-in.
if [ "$board" = bcm27xx ] && ! grep -q '^# IR input; check HAT' target/linux/bcm27xx/image/config.txt; then
 printf '\n# IR input; check HAT pin assignments before enabling.\n#dtoverlay=gpio-ir,gpio_pin=26\n' >> target/linux/bcm27xx/image/config.txt
fi
make -j"${JOBS:-8}" download
make -j"${JOBS:-8}" V=s

target_build=$(make -s --no-print-directory val.BUILD_DIR)
case "$target_build" in /build/build_dir/target-*) ;; *) echo 'Unexpected target build directory' >&2; exit 1 ;; esac
kernel_config=
for candidate in "$target_build/linux-${board}_$subtarget"/linux-*/.config; do
 [ ! -f "$candidate" ] || { kernel_config=$candidate; break; }
done
[ -n "$kernel_config" ] || { echo 'No generated kernel configuration found' >&2; exit 1; }
for setting in CONFIG_PREEMPT_NONE=y CONFIG_PREEMPT_NONE_BUILD=y CONFIG_RC_CORE=y CONFIG_BPF_LIRC_MODE2=y CONFIG_LIRC=y CONFIG_IR_GPIO_CIR=m CONFIG_IR_GPIO_TX=m CONFIG_IR_NEC_DECODER=m CONFIG_IR_RC5_DECODER=m CONFIG_IR_RC6_DECODER=m "$receiver_setting"; do
 grep -qx "$setting" "$kernel_config" || { echo "Kernel dropped required setting: $setting" >&2; exit 1; }
done
if grep -qx CONFIG_PREEMPT_RT=y "$kernel_config"; then
 echo "PREEMPT_RT must remain disabled" >&2; exit 1
fi
# Inspect the assembled filesystem as well as Kconfig: a package manifest
# alone cannot prove that the module files and player overlay were installed.
rootfs="$target_build/root-$board"
version=$(basename "$(dirname "$kernel_config")" | sed 's/^linux-//')
[ ! -e "$rootfs/lib/modules/$version/slopify-files" ] || { echo 'Invalid overlay found inside module directory' >&2; exit 1; }
for module in gpio-ir-recv gpio-ir-tx ir-nec-decoder ir-rc5-decoder ir-rc6-decoder brcmfmac brcmutil cfg80211 "$receiver_module"; do
 [ -s "$rootfs/lib/modules/$version/$module.ko" ] || { echo "Missing installed module: $module" >&2; exit 1; }
done
# Unprivileged ubusd must be able to traverse /sbin and read account data.
for path in bin sbin lib usr usr/bin usr/sbin; do
 [ "$(stat -c %a "$rootfs/$path")" = 755 ] || { echo "Invalid directory mode: /$path" >&2; exit 1; }
done
for path in etc/passwd etc/group; do
 [ "$(stat -c %a "$rootfs/$path")" = 644 ] || { echo "Invalid account file mode: /$path" >&2; exit 1; }
done
[ "$(stat -c %a "$rootfs/etc/shadow")" = 600 ] || { echo 'Invalid shadow mode' >&2; exit 1; }
cmp /player/slopify-player "$rootfs/usr/bin/slopify-player"
cmp /player/slopify-ir "$rootfs/usr/bin/slopify-ir"
[ -s "$rootfs/etc/rc_keymaps/protocols/pulse_distance.o" ] || { echo 'Missing eBPF pulse-distance decoder' >&2; exit 1; }
cmp /slopify/files/www/luci-static/resources/view/slopify/player.js "$rootfs/www/luci-static/resources/view/slopify/player.js"
[ -x "$rootfs/usr/libexec/slopify/ir-info" ] && [ -x "$rootfs/usr/libexec/slopify/ir-setup" ] || { echo 'Missing IR setup helpers' >&2; exit 1; }
# Record the target userspace ELF hashes.
userspace_list="/build/.slopify-userspace-$PROFILE_NAME.sha256"
find "$rootfs/bin" "$rootfs/sbin" "$rootfs/usr/bin" "$rootfs/usr/sbin" "$rootfs/lib" "$rootfs/usr/lib" \
 -type f ! -path "$rootfs/lib/modules/*" | sort | while IFS= read -r file; do
 magic=$(od -An -tx1 -N4 "$file" | tr -d ' \n')
 [ "$magic" = 7f454c46 ] || continue
 machine=$(od -An -tu2 -j18 -N2 "$file" | tr -d ' \n')
 [ "$machine" = "$elf_machine" ] || continue
 hash=$(sha256sum "$file" | cut -d ' ' -f 1)
 printf '%s  %s\n' "$hash" "${file#"$rootfs/"}"
done > "$userspace_list"
[ -s "$userspace_list" ] || { echo 'No target userspace found' >&2; exit 1; }
mkdir -p "/out/$PROFILE_NAME"
cp "$kernel_config" "/out/$PROFILE_NAME/kernel.config"
cp .config "/out/$PROFILE_NAME/openwrt.config"
cp "$userspace_list" "/out/$PROFILE_NAME/userspace.sha256"
git rev-parse HEAD > "/out/$PROFILE_NAME/openwrt.commit"
git diff -- "target/linux/$board" > "/out/$PROFILE_NAME/openwrt.patch"
# Include the added package as well as tracked board-file changes.
(git diff --no-index -- /dev/null package/slopify/slopify-ir/Makefile || [ "$?" -eq 1 ]) >> "/out/$PROFILE_NAME/openwrt.patch"
./scripts/diffconfig.sh > "/out/$PROFILE_NAME/openwrt.diffconfig"

git -C feeds/packages diff > "/out/$PROFILE_NAME/packages.patch"
cp feeds.conf.default "/out/$PROFILE_NAME/feeds.conf"
find "bin/targets/$board/$subtarget" -maxdepth 1 -type f \( -name '*.img.gz' -o -name '*.manifest' -o -name '*.bom.cdx.json' -o -name profiles.json -o -name sha256sums \) -exec cp {} "/out/$PROFILE_NAME/" \;
echo "Built $PROFILE_NAME with upstream CPU defaults, non-RT scheduling and IR"
