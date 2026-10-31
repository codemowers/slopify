# Shared by the Slopify init and hotplug scripts.

SLOPIFY_RUN=/var/run/slopify
SLOPIFY_STATE=/etc/slopify-player

# Player name: the OpenWrt system description, else the hostname.
slopify_name() {
	local name
	name="$(uci -q get system.@system[0].description)"
	[ -n "$name" ] || name="$(uci -q get system.@system[0].hostname)"
	[ -n "$name" ] || name="$(cat /proc/sys/kernel/hostname)"
	printf '%s' "$name"
}

# ALSA card ID. auto prefers a USB Audio Class device, then the first card
# that is not HDMI (b1 on the Pi), the Pi headphone jack or a loopback/dummy device.
slopify_card() {
	local configured
	configured="$(uci -q get slopify.player.card)"
	if [ -n "$configured" ] && [ "$configured" != auto ]; then
		printf '%s' "$configured"
		return
	fi
	slopify_auto_card
}

slopify_auto_card() {
	local dir id fallback=
	for dir in /proc/asound/card[0-9]*; do
		[ -r "$dir/id" ] || continue
		read -r id < "$dir/id"
		case "$id" in
		*[Hh][Dd][Mm][Ii]*|b[0-9]|Headphones|Loopback|Dummy) continue ;;
		esac
		if [ -e "$dir/usbid" ]; then
			printf '%s' "$id"
			return
		fi
		[ -n "$fallback" ] || fallback="$id"
	done
	printf '%s' "$fallback"
}

# Opens only the devices the configuration names to the player. A device
# plugged in later needs a player restart.
slopify_grant_devices() {
	local option device dir name rotary= ir
	ir="$(uci -q get slopify.player.ir_device)"
	[ -n "$(uci -q get slopify.player.volume_control)" ] && ir=
	[ -n "$(uci -q get slopify.player.volume_control)$ir" ] && rotary=rotary_input
	for option in $rotary playpause_input previous_input next_input mute_input; do
		device="$(uci -q get "slopify.player.$option")"
		[ -n "$device" ] || continue
		for dir in /sys/class/input/event*; do
			read -r name < "$dir/device/name" 2>/dev/null || continue
			[ "$name" = "$device" ] || [ "/dev/input/${dir##*/}" = "$device" ] || continue
			chgrp audio "/dev/input/${dir##*/}" && chmod 0640 "/dev/input/${dir##*/}"
		done
	done
	[ -c "$ir" ] && chgrp audio "$ir" && chmod 0640 "$ir"
}
