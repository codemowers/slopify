# OpenWrt speaker images

Flashable OpenWrt 25.12.6 SD-card images for the Raspberry Pi 3 (`rpi-3`) and
Cubietruck (`cubietruck`). The player is a static Go binary, compiled outside
the image and added to the official packages by the official Image Builder.
No package is compiled, and there is no mpv, ffmpeg, PipeWire or AirPlay.

```sh
SLOPIFY_SERVER=https://slopify.example.com \
SSH_AUTHORIZED_KEYS="$(curl -fsS https://github.com/USER.keys)" \
  player/openwrt/build.sh rpi-3
```

Needs Go and Docker. Images land in `dist/openwrt/<profile>/`. With no arguments,
both profiles are built. Both variables are optional; the server can be set later
with UCI. SSH is the OpenSSH server rather than Dropbear, open on the wired LAN.
`SSH_AUTHORIZED_KEYS` become root's authorized keys and turn password login off.
Without them root has no password, and OpenSSH refuses empty passwords, so **nobody
can log in over SSH** until a password is set on the console.

`.github/workflows/openwrt.yml` builds both profiles, without a server or keys, and
keeps the images as workflow artifacts.

## Device behaviour

- Formats: the speaker plays FLAC, WAV and MP3 originals. Anything else, including
  audio in video containers, fails with a visible error, because there is no mpv.
- Networking: the Ethernet interface is a DHCP and DHCPv6 client. The image has no
  DHCP/RA server, PPP or Wi-Fi supplicant or driver; any remaining radio is disabled.
  There is no firewall: this is a trusted-LAN speaker, so keep it off untrusted networks.
  Its IPv6 address is reachable if the router routes one to it.
- Clock: a Pi boots with a stale date, so TLS to the server fails until NTP has run.
  A hotplug hook restarts `sysntpd` when the LAN comes up, because its jail otherwise
  starts without DNS.
- Player name: the OpenWrt system description, falling back to the hostname, which
  a fresh image sets to `slopify`.
  Changing it restarts the services:
  `uci set system.@system[0].description='Kitchen'; uci commit system; reload_config`.
- Configuration: `/etc/config/slopify` (server, card, local UI, receivers, opt-in
  hardware volume, input devices and IR codes), also under Services → Slopify player in LuCI on port 80.
  **LuCI logs in with root's password, and root has none until you run `passwd`**, so
  until then anyone on the LAN gets root through it. It uses plain HTTP.
- Persistent pairing token and local source settings: `/etc/slopify-player/`, kept
  across sysupgrade. Adopt the device in the web app after setting the server. Without
  a server the speaker plays LAN sources only.
- Inputs are chosen by kernel device name, such as `rotary@17`, and looked up on each
  open. The panel does not list keyboards, though a name can be typed. Plugging in a device after the
  player started needs a player restart, to make it readable.
- Buttons: play/pause, previous, next and mute each take an input device and key code.
  Previous and next need a server with that control, and only move the Slopify queue.
  Mute needs hardware volume; with no playback switch it parks the level at the minimum.
- LAN audio: the player receives Pulse tunnels on TCP 4713 and advertises them
  through Avahi under the player name. There is no authentication.
- Card selection: `card auto` prefers a USB Audio Class DAC, then the first card that
  is not HDMI, the Pi headphone jack or a loopback. Plugging or removing a USB DAC restarts the player only when
  that changes the selection. Set a card ID from `/proc/asound/cards` to pin it.

### Raspberry Pi 3

USB audio, every I2S sound board driver packaged for this target, and the GPIO rotary
encoder and button drivers are installed. `profiles/rpi-3-config.txt` is appended to
`config.txt`: Wi-Fi and Bluetooth off, onboard HDMI and 3.5 mm audio available but never
chosen automatically, and the HiFiBerry DAC2 HD overlay.
To use another board, change that one `dtoverlay=` line in `/boot/config.txt` and
reboot; for example `hifiberry-dacplus-std` for a DAC+ or Amp2. A HAT's ID EEPROM names
it in `/sys/firmware/devicetree/base/hat/product`.

The knob overlays (BCM23/24, push on BCM25) are commented out in `config.txt`. For an
Amp2 with the knob, uncomment them, reboot and run:

```sh
uci set slopify.player.volume_control=Digital
uci set slopify.player.rotary_input=rotary@17
uci set slopify.player.playpause_input=button@19
uci commit slopify; /etc/init.d/slopify-player restart
```

IR amp control does not work on this image: the OpenWrt kernel is built without
`CONFIG_RC_CORE`, so `gpio-ir-tx` has no driver and no `/dev/lirc` appears.

### Cubietruck

USB audio and the onboard analog codec are installed. Wi-Fi is left out.

## Status

The Raspberry Pi 3 image boots on a 3B+ with a DAC2 HD: DHCP, OpenSSH and the sound card
come up. Playback, mDNS, the knob and USB hotplug still need verifying, and the Cubietruck
image has not been booted.
