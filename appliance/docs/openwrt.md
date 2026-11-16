# OpenWrt speaker images

The supported release devices are **Raspberry Pi 3, Pi 4 and Pi 5**, using
OpenWrt 25.12.6 SD-card images (`rpi-3`, `rpi-4`, `rpi-5`). These release images
have **no GPIO IR receive/decoding support**. LuCI explains the separate kernel
build and receiver setup needed to enable it.

Cubietruck and GL.iNet GL-AR750/AR750S profiles remain available for manual,
experimental builds; they are not supported release devices and CI does not
build or publish their images. The player is a static Go binary, compiled outside
the image and added to the official packages by the official Image Builder.
No package is compiled, and there is no mpv, ffmpeg or PipeWire. AirPlay is the player's own
receiver; with its encryption enabled, `/usr/libexec/slopify/raop-key` downloads the RAOP key from
the shairport-sync repository after the LAN comes up, keeps it in `/etc/slopify-player`, and restarts
the player. The player accepts only the genuine key.

```sh
SLOPIFY_SERVER=https://slopify.example.com \
SSH_AUTHORIZED_KEYS="$(curl -fsS https://github.com/USER.keys)" \
  appliance/build.sh rpi-3
```

Needs Go and Docker. Images land in `dist/openwrt/<profile>/`. With no arguments,
the three supported Pi profiles are built. Pass experimental profile names explicitly. Both variables are optional; the server can be set later
with UCI. SSH is OpenWrt's Dropbear, open on the wired LAN. `SSH_AUTHORIZED_KEYS`
become `/etc/dropbear/authorized_keys` and turn SSH password login off. OpenWrt's
Dropbear accepts Ed25519, FIDO `sk-ssh-ed25519` and RSA keys only, not ECDSA or
`sk-ecdsa`. Keys can also be managed later in LuCI under
System → Administration → SSH-Keys. ttyd provides a web shell (LuCI → Services →
Terminal, port 7681) that runs `/bin/login` on the LAN interface over plain HTTP.
As on stock OpenWrt, root has no password until one is set, so LuCI and the web
shell are open to the LAN until then: set it in LuCI first.

`.github/workflows/openwrt.yml` builds only the three supported Pi profiles when a tag is pushed, without a server
or keys, and keeps the images as workflow artifacts. Each job compiles the static player and separate receiver setup tool on
an Ubuntu runner, then invokes `openwrt/imagebuilder:<target>-25.12.6` with Docker
through `build.sh`. CI never runs `source-build.sh`, compiles a kernel, or uses
an SDK. The separate `player.yml` workflow builds standalone Linux Go binaries
on tag pushes. Neither workflow publishes a GitHub Release automatically.

| Profile | Official Image Builder target | Player architecture |
|---|---|---|
| `rpi-3` | `bcm27xx-bcm2710-25.12.6` | ARM64 |
| `rpi-4` | `bcm27xx-bcm2711-25.12.6` | ARM64 |
| `rpi-5` | `bcm27xx-bcm2712-25.12.6` | ARM64 |

Each Pi uses its own upstream kernel, firmware and device profile; there is no
shared cross-model kernel. The [official device definitions](https://github.com/openwrt/openwrt/blob/v25.12.6/target/linux/bcm27xx/image/Makefile)
provide these profiles. Pi 4/5 playback and HAT compatibility still need hardware
verification. To build only the Pi release images locally:

```sh
appliance/build.sh rpi-3 rpi-4 rpi-5
```

## Manual experimental builds: Cubietruck and AR750S

These targets are excluded from release CI and published artifacts. Go and
Docker are required, just as for the Pi build. No OpenWrt SDK or source build
is needed for their stock Image Builder images:

```sh
appliance/build.sh cubietruck
appliance/build.sh gl-ar750s
```

Outputs are in `dist/openwrt/cubietruck/` and `dist/openwrt/gl-ar750s/`.
Cubietruck uses `openwrt/imagebuilder:sunxi-cortexa7-25.12.6` and an ARMv7
player; AR750S uses `openwrt/imagebuilder:ath79-nand-25.12.6` and a soft-float
MIPS player. `SLOPIFY_SERVER`, `SSH_AUTHORIZED_KEYS`, `EXTRA_PACKAGES` and
`PLAYER_BUILD_TAGS` also apply to these explicit builds. The old `gl-ar750`
profile is retained for manual experimentation only.

Cubietruck's current S/PDIF driver exposes only S16_LE, and its onboard IR driver
does not establish working decoding. AR750S images omit native Vorbis on
soft-float MIPS. See the board notes below before using either experimental image.

## Device behaviour

- Formats: the speaker plays FLAC, WAV and MP3 originals; Pi/Cubietruck also include mono/stereo Ogg Vorbis. Anything else, including
  audio in video containers, fails with a visible error, because there is no mpv.
- Networking: Pi/Cubietruck bind LAN directly to `eth0` as a DHCP and DHCPv6 client
  and remove the stock `br-lan` bridge. GL routers bridge all Ethernet ports instead. The image has no
  DHCP/RA server or PPP. Onboard Wi-Fi drivers, board firmware, WPA support and
  LuCI wireless tools are included; fresh images leave Wi-Fi unconfigured.
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
  hardware volume, input devices and IR codes), also under Services → Slopify player in LuCI on port 8080. The player page is on port 80.
  **LuCI logs in with root's password, and root has none until you run `passwd`**, so
  until then anyone on the LAN gets root through it. It uses plain HTTP.
- Persistent pairing token and local source settings: `/etc/slopify-player/`, kept
  across sysupgrade. Adopt the device in the web app after setting the server. Without
  a server the speaker plays LAN sources only.
- Standard multimedia keys are read from every `/dev/input/event*` device at
  startup, without an exclusive grab. OpenWrt grants audio-group read access
  and passes the expanded list to the player. Input hotplug coalesces changes
  and restarts the running player; this interrupts playback. Ordinary keys
  and mouse movement are ignored. The separate Rotary tab selects the one
  relative-axis encoder allowed to control volume; its GPIO overlay is opt-in.
- Previous and next control the selected AirPlay sender when supported,
  otherwise the queue through the server. Mute needs hardware volume and stays
  local; without a playback switch it parks the level at the minimum.
- LAN audio: the player receives Pulse tunnels on TCP 4713 and advertises them
  through Avahi under the player name. There is no authentication.
- Card selection: `card auto` prefers a USB Audio Class DAC, then the first card that
  is not HDMI, the Pi headphone jack or a loopback. Plugging or removing a USB DAC restarts the player only when
  that changes the selection. Set a card ID from `/proc/asound/cards` to pin it.

### Raspberry Pi 3/4/5

USB audio, every I2S sound board driver packaged for this target, and the GPIO rotary
encoder and button drivers are installed. `profiles/rpi-3-config.txt` is appended to
`config.txt`: Wi-Fi available and Bluetooth off, onboard audio available where supported by the board, but never
chosen automatically. Pi 5 has no onboard 3.5 mm jack. Audio HAT selection defaults to Auto: Raspberry Pi firmware
tries the HAT EEPROM at boot. This is Pi firmware behavior, not generic Linux
hardware detection; missing EEPROM data or firmware mappings require an override.

In LuCI → Services → Slopify, **Audio HAT overlay** offers Off, Auto (with the
EEPROM identity and active sound device when available), and installed audio
`.dtbo` overlays. Lua helpers filter overlays for the `#sound-dai-cells` property
(the same filter moOde uses), with its Cirrus exception. No DTB handling lives in Go.
Save and reboot to apply. Off suppresses HAT overlays without disabling USB or
onboard audio. Existing explicit choices are preserved. Custom conditional or
parameterised audio declarations must be edited manually; the helper refuses to
rewrite them. It checks for concurrent edits and backs up the previous file as
`/boot/config.txt.slopify-backup`.

For Amp4 use `hifiberry-dacplus-std`: the generic `hifiberry-dacplus` overlay can
misdetect Pro clocking and leave the amplifier silent. The tested Amp4 exposed no
EEPROM identity, so it needs this explicit override.

The knob overlays (BCM23/24, push on BCM25) are commented out in `config.txt`. For an
Amp2 with the knob, uncomment them, reboot and run:

```sh
uci set slopify.player.volume_control=Digital
uci set slopify.player.rotary_input='rotary@17'
uci commit slopify; /etc/init.d/slopify-player restart
```

GPIO IR reception and transmission are unavailable in the official Pi Image
Builder images: the kernel lacks RC-core. An overlay or extra userspace package
cannot add a kernel feature that was never built. LuCI keeps **IR receive
(unavailable)** gray and its settings read-only when kernel RC support or
`ir-keytable` is absent. The tab remains clickable to explain the required setup;
stored mappings are preserved when booting a release image. Standard USB/media
keys continue to work independently of GPIO IR.

### Experimental ARM64 Pi source build

**Previous Pi 3 RT build has a known boot failure on a Pi 3B+: kernel crashes
and resets, with USB/network unavailable. Do not use it on a working speaker.**
The config still selects legacy `CONFIG_USB_DWCOTG=y` and disables `DWC2`.
[Upstream reports](https://github.com/raspberrypi/linux/issues/5809) document
ARM64 Pi 3 RT problems in this driver path. The exact local panic needs its
full trace; the clock warning or `mld_ifc_work` name alone does not identify it.
The source workflow now restores the pinned upstream board configuration and
adds IR through a KernelPackage, retaining normal non-RT scheduling, clock and
USB settings. Replacement images require hardware validation; IR does not
require PREEMPT_RT. The replacement non-RT images have booted on Pi 3 HiFiBerry speakers and
Cubietruck. Amp4 and Cubietruck expose LIRC with the pulse-distance BPF decoder
attached. This does not verify every remote button or Pi 4/5 hardware.

`source-build.sh` is a separate, opt-in source build using OpenWrt 25.12.6
(commit `64900e756efa667774214f9c6b17c113222e01f7`) and its pinned package/LuCI
feeds. It builds in an unprivileged Debian container and compiles the static
Slopify player on the host. Docker, Go and Git are required; host compiler
packages are not installed. The default Image Builder workflow is unchanged.

```sh
# ARM64 Pi 3 family only.
JOBS=8 appliance/source-build.sh rpi-3
```

The profile covers Pi 3/3B+/3A+/CM3/Zero 2 (and Pi 2 revision 1.2).
It retains OpenWrt's upstream CPU, toolchain and optimization defaults.
Pi 4/5 are outside this workflow; OpenWrt selects one subtarget per build
configuration. `userspace.sha256` records the ARM64 userspace ELF hashes.

Build state lives under `.build-tmp/openwrt-rpi3` (`SOURCE_WORKDIR` overrides
it); images, manifests, source/feed pins and the actual generated kernel config
are copied to `dist/openwrt-ir/rpi-3` (`OUT` overrides its parent).
`openwrt.patch` includes the added IR package; `openwrt.diffconfig` records
OpenWrt configuration differences from defaults. The build normalizes tracked
checkout permissions and uses umask 022, preventing restrictive host permissions
from leaking into standard image directories. Private keys remain restricted.
Use disk-backed storage with substantial free space; builds retain source,
download and compiler state between runs. A target will not start with less
than 8 GiB free. `JOBS` defaults to 8. `SLOPIFY_SERVER`, `SSH_AUTHORIZED_KEYS`,
`EXTRA_PACKAGES` and `PLAYER_BUILD_TAGS` have the same role as in Image Builder.

Kernel options come from OpenWrt package definitions, with no separate kernel
fragment or interactive kernel configuration. The local `kmod-slopify-ir` package includes RC-core, LIRC,
GPIO receive/transmit and NEC/RC5/RC6 decoders. RC-core is built-in because
Linux requires that for `CONFIG_BPF_LIRC_MODE2`; the v4l-utils feed patch enables
upstream BPF IR decoding and installs its protocol objects. `v4l-utils` supplies `ir-keytable`,
`ir-ctl` and upstream userspace keymaps. The build rejects a generated kernel
configuration that enables RT or drops any required IR setting. This check establishes
configuration, not on-device timing, electrical compatibility or working IR.
Custom kernel modules must match this build; stock OpenWrt kernel module
packages cannot be assumed compatible.

At player startup, all input event devices receive audio-group read
access; a configured LIRC transmitter receives audio-group read/write access.
Input hotplug restarts a running player. GPIO overlays stay commented out. Enable `gpio-ir` for reception and
`gpio-ir-tx` for transmission only after checking the HAT and wiring; BCM17 is
not safe for IR output on Beocreate. Reception on BCM26 remains a proposal,
not a universal wiring default. After boot, inspect `/proc/config.gz`,
`ir-keytable`, `/dev/input/event*` and `/dev/lirc*`, then verify actual input and
native-rate playback under load. No hardware is flashed by
the build script.

### How the HiFiBerry Amp4 setup gained IR

See the [Amp4 IR receiver example](hifiberry-amp4/README.md) for the tested
GPIO25 wiring, audio/receiver overlays, kernel/BPF requirements and LuCI learning.
The [Amp2 example](hifiberry-amp2/README.md) is the separate rotary setup.

### Remote-control input in LuCI

On release images, open **Services → Slopify player → IR receive (unavailable)**
for the requirements. LuCI cannot rebuild the kernel or enable the GPIO overlay.
After installing an IR-capable kernel with matching modules and `ir-keytable`,
edit `/boot/config.txt` to enable the checked receiver pin and reboot. In
**General**, select `hifiberry-dacplus-std` for Amp4 and reboot after changing it.
In **Volume control**, select the card's `Digital` control for volume/mute.

Once kernel RC support and the tool are present, Services → Slopify player →
IR receive selects a receiver and protocol. Save &
Apply once to enable decoding, even with no bindings. Each action row shows its
standard Linux media key and provides Learn, Clear and an editable scancode.
Click Learn, press and release the remote button, then Save & Apply the bindings.
Learning lasts at most five seconds, uses full 64-bit LIRC scancodes, and grabs
only that input device so the learning press cannot also trigger playback or
volume. It never changes the active keymap. RECS80 toggles are canonicalized;
Denon learning rejects the wrong vendor or invalid frame parity. There is no
listener or automatic replay; concurrent learning fails busy.

Save & Apply writes the UCI settings, replaces the selected receiver's kernel
keymap with the configured actions using `ir-keytable`, and restarts the player.
All event devices receive audio-group read access for standard media keys. The receiver is stored
by physical device identity, not its changing `rcN` index. A missing receiver or
failed keymap setup is reported in the system log; other playback/control paths
remain available. Input hotplug restarts the running player and reapplies the receiver keymap.

Volume keys use the same local amplifier control as the rotary knob and repeat
while held. Transport and mute act on presses, without auto-repeat. IR reception is configured by the separate `slopify-ir` setup tool; the player
consumes Linux media keys and has no IR decoding or transmission code.
The RECS80/RECS80EXT and Denon Kaseikyo profiles use the upstream BPF
pulse-distance decoder, loaded by ir-keytable. They require built-in RC-core
and CONFIG_BPF_LIRC_MODE2; an older module-only RC-core image cannot load them.
RECS80 toggle variants map to the same action. Denon uses its full 48-bit
scancode. The Denon PMA-60 capture matches Kaseikyo vendor 0x3254 with valid
parity; the TechniSat 100 TS 009 produces RECS80 scancodes on the Pi, with toggle
variants verified. Individual button labels still require recording.

Receive-carrier fields appear only for devices advertising the LIRC carrier
filter capability. A tunable range can be entered as 36 and 38 kHz. Ordinary
GPIO-connected demodulating receivers have a hardware-fixed carrier frequency;
software cannot retune a 38 kHz receiver to 36 kHz. Choose suitable receiver
hardware and enable its device-tree overlay before selecting it in LuCI. No
GPIO overlay or pin is enabled automatically by the remote-control settings.

`slopify-ir ir-info` provides bounded receiver inventory for the LuCI helper;
`ir-setup` validates the configured mappings and invokes `ir-keytable` with a
five-second deadline. Root-only `ir-learn` is allowed by the write ACL for
LuCI configuration sessions. These are local commands, with no new listener.

### Future custom kernel build

The default Image Builder workflow cannot enable missing kernel features.
The experimental ARM64 Pi source workflow above adds IR support with normal
non-RT scheduling. On-hardware verification and other targets remain separate
work:

- **PREEMPT_RT:** dropped after Pi 3 boot failures. Do not re-enable it without
  a new explicit request and hardware validation. IR does not depend on RT.
- **Cubietruck S/PDIF precision:** include upstream `sun4i-spdif` fixes
  [80ac12ffb3a9](https://github.com/torvalds/linux/commit/80ac12ffb3a9)
  (MSB alignment) and
  [6e750d3ec741](https://github.com/torvalds/linux/commit/6e750d3ec7410c8d3aa6a006d37142eb837b3c03)
  (format bitmask and DMA width for working 24-bit output). Verify S24_LE and
  S32_LE negotiation and actual sample alignment on hardware; S32 transport
  over S/PDIF does not imply 32-bit wire precision. Both fixes first appear
  in Linux 6.14. OpenWrt 25.12.6 on Cubietruck runs Linux 6.12.112; the
  upstream 6.12.112 source and inspected 6.12 stable branch lack both fixes.
  Use a kernel containing them or backport them explicitly.
- **IR reception and transmission:** enable RC-core (`CONFIG_RC_CORE`), LIRC
  (`CONFIG_LIRC`), GPIO reception (`CONFIG_IR_GPIO_CIR`), GPIO transmission
  (`CONFIG_IR_GPIO_TX`), and the protocol decoders needed by supported remotes
  (at least NEC; include RC5/RC6 when used). Package the matching modules and
  `ir-keytable`, provide remote keymaps and opt-in device-tree configuration.
  Verify receive events through `/dev/input/event*` and transmission through
  `/dev/lirc*` on hardware. The official Pi kernel currently has RC-core disabled.

Keep hardware verification separate from build configuration; an Image Builder
rebuild does not supply RT or IR support. GPIO assignments must avoid each audio HAT's
reserved pins and existing controls. BCM26 is the proposed receiver input for
Amp2, Amp4 and DAC2 HD; confirm the receiver's supply voltage and pinout first.

### Cubietruck (manual, experimental)

USB audio, the onboard analog codec and S/PDIF output drivers are installed.
S/PDIF needs `kmod-sound-soc-sunxi-spdif` in addition to the analog codec package;
it pulls in the S/PDIF codec driver. Select its ALSA card in LuCI after boot.
The current official 6.12.112 driver exposes only S16_LE on the tested board.
By default the player therefore rejects sources requiring more than 16 bits
instead of silently truncating them. An explicit workaround is
`uci set slopify.player.allow_truncate=1; uci commit slopify; /etc/init.d/slopify-player restart`.
This permits a final 16-bit fallback for all native ALSA audio, discarding low
bits without resampling or dithering. All images, including Cubietruck, ship
with `allow_truncate=0`; LuCI exposes the setting as **Allow 16-bit fallback**. Installing the driver enables the card but does not
fix this precision limitation; see the custom-kernel backports above.
Audible S/PDIF playback has been confirmed on Cubietruck. The profile and generated image manifests include `kmod-sound-soc-sunxi-spdif`.
The onboard Broadcom Wi-Fi driver and BCM43362 firmware are included; its
board calibration file comes from the official sunxi base files.

The onboard IR receiver uses the official `kmod-sunxi-ir` package. On the
tested stock 25.12.6 kernel, installing it with `kmod-input-evdev` registers
`sunxi-ir` and an input event node, but `ir-keytable` reports no supported
kernel protocols and the empty `rc-empty` keymap. No decoder packages are
available in the configured feeds, and no LIRC node appears. Driver
installation alone therefore does not provide usable remote buttons; a
kernel build enabling the required protocol decoders is still needed.

#### Cubietruck IR test image

The manual source workflow also accepts Cubietruck, in a separate checkout from
Pi builds:

```sh
JOBS=4 appliance/source-build.sh cubietruck
```

It uses pinned OpenWrt 25.12.6 `sunxi/cortexa7` board defaults, the onboard
`sunxi-cir` receiver, built-in RC-core/LIRC, NEC/RC5/RC6 modules and v4l-utils
with its eBPF pulse-distance decoder. The separate `slopify-ir` setup tool backs
LuCI's receiver selection and button learning. There is no player IR transmitter.
Build state lives under `.build-tmp/openwrt-cubietruck`; artifacts and actual
kernel configuration go to `dist/openwrt-ir/cubietruck/`. `SOURCE_WORKDIR` and
`OUT` can select other disk-backed locations. Keep one profile per checkout;
custom modules must match this custom kernel.

This is an experimental test image, excluded from release CI. Building and
checking modules does not establish that remote buttons work on hardware.
After boot, check `ir-keytable`, `/dev/lirc*` and `/dev/input/event*`, then select
the onboard receiver and remote protocol in LuCI → Services → Slopify player →
IR receive. Save & Apply, learn each button and Save & Apply again. The S/PDIF
precision limitation and default-disabled 16-bit fallback remain unchanged.

## Wi-Fi setup

Connect by Ethernet first, then use LuCI **Network → Wireless → Scan → Join**
to configure a station on your existing network. Use a separate DHCP client
interface (for example `wwan`); wired LAN retains its profile default (direct
`eth0` on Pi/Cubietruck, `br-lan` on GL routers).
Set the radio country to your location. No SSID or credentials are baked in.
Existing wireless configuration is preserved by the network defaults.
Upgrades retaining a Pi boot config with `dtoverlay=disable-wifi` need that old
line removed before Wi-Fi can appear; the new image config omits it.

### GL.iNet GL-AR750 and GL-AR750S

Manual experimental targets only; excluded from supported releases and CI artifacts.

```sh
appliance/build.sh gl-ar750 gl-ar750s
```

These profiles use big-endian MIPS with `GOMIPS=softfloat`. Native Vorbis is
excluded automatically; FLAC, WAV, MP3 and ALAC remain compiled in. There is no
mpv. `PLAYER_BUILD_TAGS='no_alac no_mp3'` can omit additional native decoders;
see [the player build options](../../player/README.md#decoder-build-options).

All three Ethernet sockets, including the socket labelled WAN, become LAN switch
ports. The AR750 bridges its separate WAN Ethernet controller into the LAN;
the AR750S puts all external switch ports in one VLAN. `br-lan` gets its own
DHCP/DHCPv6 address. IP forwarding is disabled; there is no NAT or DHCP/RA server.
Wi-Fi drivers and LuCI wireless setup remain available. To use Wi-Fi, join your
existing access point in LuCI with a DHCP client interface. Ordinary station mode
does not transparently bridge the wired ports onto the upstream Wi-Fi network.

Connect a USB Audio Class DAC, such as the Schiit Modi 3, to the USB host port.
The existing `card auto` selection follows USB DAC hotplug. No hardware mixer is
configured by default; the Modi 3 uses the downstream amplifier's volume control.
Output retains native sample rate and precision; unsupported formats fail unless
16-bit fallback was explicitly enabled. Playback through a physical Modi 3 still
needs hardware verification.

The AR750 has only 16 MB flash. The AR750S image targets **NOR/NAND**, not the
NOR-only layout. Outputs are `*-squashfs-sysupgrade.bin` plus an initramfs kernel
for recovery/testing where supported. Follow the device's OpenWrt installation
procedure when coming from vendor firmware; these are not SD-card images.

Both routers have 128 MB RAM. First-boot defaults set `read_ahead_mib=8` and
`memory_limit=32MiB` in the player UCI section. The Go soft memory target is raised
by the read-ahead budget; it is not an RSS cap. Existing explicit settings survive
sysupgrade. Other boards keep their previous defaults.
Player Prometheus instrumentation defaults off on MIPS (`-metrics=true` opts in).
LuCI offers Automatic/Enabled/Disabled through `slopify.player.metrics`; empty
uses the architecture default, `0` disables and `1` enables it. Disabling skips
instrumentation and TCP sampling as well as removing the player's `/metrics`.
Speaker status and integer PCM peak charts remain available.

## Status

The Raspberry Pi 3 image boots on a 3B+ with a DAC2 HD and a Pi 3 with an Amp2. On the Amp2,
Pulse tunnels discovered over mDNS play audio. The knob and USB hotplug still need verifying.
Cubietruck playback works after adding the S/PDIF driver, which is included in
the generated images. S/PDIF retains the driver precision limitation described above.

## Receive multicast from a laptop

Enable **RTP multicast** in LuCI's receiver settings, or run on the speaker:

```sh
uci set slopify.player.rtp=1
uci commit slopify
/etc/init.d/slopify-player restart
```

On a PipeWire-Pulse laptop, open the speaker page’s **Multicast RTP** tab and
copy the setup snippet. It derives a stable group from a prefixed machine-ID
hash and configures native RTP and SAP modules without binding them to an
interface, so they follow the current route. Both are `nofail`: a multicast
problem leaves the rest of PipeWire-Pulse running. Audio goes to that group on UDP 5004; discovery remains on
224.0.0.56:9875. The output label includes the audio group in brackets.

Select that output in laptop sound settings and move already-running applications
if needed. Select the RTP source on the speaker page if another source is pinned.
The same tab includes copyable removal commands. A VPN that owns the default
route also carries the multicast stream; add `local.ifname` to both modules to pin
an interface.

### Stable sender groups

The OpenWrt image generates `slopify.player.rtp_send_group` once as
`239.192.x.y:5004` using kernel randomness and commits it to UCI. Startup preserves
existing values, including user overrides and settings restored by sysupgrade.
LuCI → Services → Slopify → Receivers exposes the group when Multicast sender is
enabled. An address without a port uses 5004. Generating a group does not enable
transmission; `rtp_send` remains off until selected.

The speaker page’s persistent PipeWire setup derives a stable laptop group from
SHA-256 of `slopify-rtp-v1:` plus `/etc/machine-id`, taking two digest bytes for
`239.192.x.y`. The raw machine ID is never announced. Edit `destination.ip` in
`30-slopify-rtp.conf` for an override; rerunning setup restores the derived value.
Cloned IDs and hash collisions can share groups. Receivers separate different
sender IPs on the same group, including identical stream names and SSRCs, and
show their IPs on separate selectable cards. Multiple simultaneous streams from
the same IP on the same group are not supported. DHCP changes can create a new
card because source identity still includes IP.

Native PipeWire RTP and SAP modules keep discovery on `224.0.0.56:9875` while
sending audio to the derived group. `module-rtp-send` sends SAP to its audio destination too, so simply
changing its destination would prevent automatic discovery by this receiver.
The UI therefore provides a single native PipeWire setup for consistent
addresses and discovery. Neither module binds to an interface: an interface
recorded at setup disappears when a laptop undocks, and a mandatory module that
cannot bind stops PipeWire-Pulse entirely.

SSH login replaces the stock banner with Slopify ASCII art, player status, sound card,
speaker/LuCI URLs, and service/log commands. The stock shell profile and its
filesystem safety warnings remain intact; the package-manager cheatsheet is suppressed.

Multicast setup only writes `30-slopify-rtp.conf`; it coexists with unicast
`30-slopify.conf` and discovery drop-ins. Its native RTP output locks volume/mute at unity, without a separate null sink. Applications may still change their own gain; receiver
hardware volume remains independently controllable.

Startup hardware volume defaults to 10 on the alsamixer displayed scale; LuCI can override it. RTP receiver reporting uses UDP at the audio port plus one. The local output diagram shows reporting receivers individually, not every listener.


Players advertise their own web UI via `_slopify._tcp`; sibling links below the
page title prefer `.local` hostnames and open in new tabs. The player calls Avahi over D-Bus directly. Its ujail launcher drops to
nobody:audio, retaining only the low-port binding capability. The default HTTP port pair migrates once from player 8080 / LuCI 80
to player 80 / LuCI 8080. Custom bindings and LuCI HTTPS are preserved.

OpenWrt LuCI exposes Pulse, ESound and multicast RTP input buffer settings, plus
the ESound socket receive queue and the shared ALSA output buffer. Empty fields
retain protocol defaults; Save & Apply restarts the player. Multicast RTP sends
and receives uncompressed PCM (L16/L24). Images include `kmod-tcp-bbr`, whose
module autoload and sysctl settings select BBR persistently for TCP connections;
this does not change UDP multicast transport.

Images pin each CPUFreq policy to its lowest supported frequency at boot through
`slopify-cpu`, before the player starts. Both scaling limits are set to
`cpuinfo_min_freq`; the powersave governor is selected when available. This is
600 MHz on the supported Raspberry Pi 3 boards; other boards use their own
reported minimum. It does not change firmware voltage or clock tables. This
reduces CPU headroom for decoding and concurrent streams.

The default is `slopify.cpu.minimum=1`. To restore the kernel's normal frequency
policy, set `uci set slopify.cpu.minimum=0`, run `uci commit slopify`, and reboot.
Disabling the setting alone does not undo limits already applied in this boot.
Boards without CPUFreq policies are left unchanged.
