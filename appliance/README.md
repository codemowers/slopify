# Slopify appliance

OpenWrt images that turn a Raspberry Pi into a Slopify speaker with a local
status page and LuCI setup for networking, audio hardware and controls.

## Goals and features

- First-class release images for Raspberry Pi 3, Pi 4 and Pi 5.
- A static Go player, direct ALSA output and optional LAN audio receivers.
- Ethernet DHCP by default, optional Wi-Fi setup and audio HAT selection in LuCI.
- Hardware volume, rotary input and standard Linux media buttons.
- Official OpenWrt Image Builder containers for release builds.

Pi 3 playback has been exercised on hardware; Pi 4/5 still need hardware playback
verification. Cubietruck and GL-AR750/AR750S are manual experimental targets and
are excluded from releases.

## Install or build

Download the matching factory or sysupgrade image from GitHub Releases. Keep a
configuration backup before upgrading. See the
[OpenWrt guide](docs/openwrt.md) for installation and setup.

To build release-style images from the repository root, install Go and Docker:

```sh
appliance/build.sh                 # Pi 3, 4 and 5
appliance/build.sh rpi-3           # One board
```

Images are written to `dist/openwrt/`. Set your server in LuCI after boot and
adopt the speaker in the browser UI. Images contain no Wi-Fi credentials or
installation-specific server settings by default.

LuCI separates Slopify server and queue sharing settings in Sources, local sound
card/HAT and multicast output settings in Sinks, and incoming LAN audio protocols
and their input buffers in Receivers. Speaker identity, the web UI and monitoring
remain in General; hardware volume and physical controls have their own tabs.

## Physical controls

Release Image Builder kernels do not support GPIO IR reception. LuCI disables
IR receive settings when that support is missing. Experimental source-built
Pi 3 and Cubietruck images provide kernel/BPF decoding outside the Go player.

See the [Amp2 rotary example](docs/hifiberry-amp2/README.md),
[Amp4 IR receiver example](docs/hifiberry-amp4/README.md) and
[experimental build instructions](docs/openwrt.md#experimental-arm64-pi-source-build).
