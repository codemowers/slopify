include $(TOPDIR)/rules.mk
include $(INCLUDE_DIR)/kernel.mk
PKG_NAME:=slopify-ir
PKG_RELEASE:=1
PKG_LICENSE:=GPL-2.0-only
PKG_BUILD_DIR:=$(KERNEL_BUILD_DIR)/slopify-ir

include $(INCLUDE_DIR)/package.mk

# These are in-tree kernel modules, already compiled by target/linux.
define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef

define Build/Configure
endef

define Build/Compile
endef

# This package is used only by our source builds, never Image Builder.
define KernelPackage/slopify-ir
  SUBMENU:=Other modules
  TITLE:=Slopify GPIO infrared receive/transmit and decoders
  DEPENDS:=@(TARGET_bcm27xx||TARGET_sunxi) +TARGET_sunxi:kmod-sunxi-ir +kmod-input-core +kmod-input-evdev
  KCONFIG:= \
	CONFIG_MEDIA_SUPPORT=y \
	CONFIG_MEDIA_RC_SUPPORT=y \
	CONFIG_RC_CORE=y \
	CONFIG_BPF_LIRC_MODE2=y \
	CONFIG_LIRC=y \
	CONFIG_RC_MAP=n \
	CONFIG_RC_DECODERS=y \
	CONFIG_RC_DEVICES=y \
	CONFIG_IR_GPIO_CIR=m \
	CONFIG_IR_GPIO_TX=m \
	CONFIG_IR_NEC_DECODER=m \
	CONFIG_IR_RC5_DECODER=m \
	CONFIG_IR_RC6_DECODER=m \
	CONFIG_IR_IMON_DECODER=n \
	CONFIG_IR_MCE_KBD_DECODER=n \
	CONFIG_IR_RCMM_DECODER=n \
	CONFIG_IR_SANYO_DECODER=n \
	CONFIG_IR_SHARP_DECODER=n \
	CONFIG_IR_XMP_DECODER=n \
	CONFIG_IR_PWM_TX=n \
	CONFIG_IR_SPI=n
  FILES:= \
	$(LINUX_DIR)/drivers/media/rc/gpio-ir-recv.ko \
	$(LINUX_DIR)/drivers/media/rc/gpio-ir-tx.ko \
	$(LINUX_DIR)/drivers/media/rc/ir-nec-decoder.ko \
	$(LINUX_DIR)/drivers/media/rc/ir-rc5-decoder.ko \
	$(LINUX_DIR)/drivers/media/rc/ir-rc6-decoder.ko
  AUTOLOAD:=$(call AutoProbe,rc-core ir-nec-decoder ir-rc5-decoder ir-rc6-decoder gpio-ir-recv gpio-ir-tx)
endef

define KernelPackage/slopify-ir/description
 Kernel RC-core and LIRC support with GPIO input/output and NEC, RC5 and RC6.
 GPIO device-tree overlays remain opt-in; no pins are claimed by this package.
endef

$(eval $(call KernelPackage,slopify-ir))
