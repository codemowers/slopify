# Slopify player

The speaker side of Slopify: a single Go binary that drives a DAC over wired Ethernet.

## Goals

1. **Conversion-free local playback from a Slopify server**, in the role squeezelite
   fills for Squeezebox, using Slopify's own HTTP protocol. The server keeps the queue;
   the player fetches originals and plays them at their native rate and precision.
2. **A PipeWire audio sink for Linux (Ubuntu) users.** A laptop's PipeWire or
   PulseAudio sends its audio to the speaker over a Pulse tunnel.
3. **An audio sink for Mac and iOS users.** Not implemented, and not part of the
   static binary yet.

Ideally all of this is one statically linked binary that runs the same on OpenWrt,
in Docker or anywhere else, over Ethernet. A Bluetooth audio sink may come later, if
it can be made to work reliably.

The purpose is music playback. Audio-video sync and gaming are not priorities, so sound
quality and reliable playback win over low latency when they conflict.

## Status

| Goal | State |
|---|---|
| Slopify playback | Works. FLAC, WAV and MP3 are decoded in Go; other formats need mpv, which is not in the static builds |
| Linux sink | Works on a Raspberry Pi 3 with a HiFiBerry Amp2. New, with little listening time. No authentication: trusted LAN only |
| ESound source | Opt-in with `-esound-listen`. Streams only, no authentication: trusted LAN only. Protocol tests pass; not yet tried with a real client |
| UPnP/DLNA renderer | Opt-in with `-upnp-listen`, for controllers such as swyh-rs. Not yet tried with a real controller. No authentication: trusted LAN only |
| Mac/iOS sink | Not implemented |
| Bluetooth sink | Not started |
| OpenWrt images | Build, but have not been booted on hardware |
| Docker image | Builds, but has not played audio |

## Design rationale

**Conversion-free.** A Slopify original reaches the DAC at its own sample
rate and full decoded precision. The only changes allowed are lossless: packing 16 or
24 bits into a wider word, or duplicating mono, to suit the hardware. An unsupported
rate is an error, never a silent resample. Sender desktop mixes under goal 2 are a
weaker guarantee, since the sender has already mixed them. The speaker reports what it
received and adds no conversion of its own.

**No pipelines.** Chaining a decoder, a pipe and a player, or running a sound server,
is fragile:

- There are too many moving parts, and each can stall, restart or lose sync on its own.
- They bring many unexpected corner cases, such as format changes, gapless transitions,
  seeks, underruns and handing the device between sources.
- It is hard to enforce no DSP and no resampling across several programs, each with
  its own defaults and negotiation. One setting left unchecked can convert audio with
  no error, and it can go unnoticed.

So one program owns the whole path, from network to ALSA `hw:` device. It decides
what opens the device and at what format, and it can refuse what it cannot play exactly.

**Memory safety.** mpv and ffmpeg pull in many libraries, and on the whole they are
written in languages that are not memory safe, while parsing complex media from the
network. This program is written in Go and intends to be memory safe, with CGO disabled
and its own decoders and Pulse server. `unsafe` appears only in `internal/alsa`, for the kernel's
ioctl interface. Decoders and network parsing do not use it. mpv remains an optional fallback for other codecs on
installs that choose to add it, and it is not part of the static builds.

**In practice:**

- Audio goes straight to an ALSA `hw:` device, with no sound server in between.
- No resampling, mixing, DSP or software volume, and no floating point in the audio
  path.
- Only one source reaches the DAC at a time. The others stay metered and unheard.
- Volume changes the hardware control, when one is configured.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o slopify-player ./player
```

Run this from the repository root, because the player shares `internal/httpx` and
`internal/volume` with the servers. The result is static and needs no libasound.

## Install

- [Ubuntu 26.04 on a Raspberry Pi](deploy/README.md), under systemd.
- [OpenWrt images](openwrt/README.md) for the Raspberry Pi 3 and Cubietruck.
- `build/player.Dockerfile`, built from the repository root. A container needs
  `/dev/snd`, and the input devices if a knob is used.

The player needs `SLOPIFY_SERVER`, then adoption in the web app. With `-local-listen`
it also serves a speaker page and `/metrics` on the LAN, with no login.

## Amplifier IR control

For an amplifier whose volume is a motorised knob, such as a NAD C326BEE fed by a line
output. **This is untested on real hardware**, and the default codes come from another NAD
model, apart from power off, which is unconfirmed. It is one-way: the speaker counts steps
from the bottom of the knob and can never read the real volume.

After `-ir-idle-off` of silence it holds volume down for `-ir-sweep`, then powers the amp
off. When audio returns it powers on, waits `-ir-wake`, and steps up to the saved level,
so music fades in. A lost command leaves it quieter, never louder, and `-ir-max-steps` caps
the climb. The knob, and + and - on the speaker page, change the saved level. Turning the
amp's own knob by hand is not seen.

It cannot tell which amp input is selected, so going idle also silences and powers off
anything else playing through that amp.

```
# /boot/firmware/config.txt, then reboot. GPIO 17 is physical pin 11, ground pin 9.
dtoverlay=gpio-ir-tx,gpio_pin=17
```

Check the jack's idle voltage before wiring it: Pi pins only tolerate 3.3 V. The kernel
must also have NEC encoding available (`ir-nec-decoder`).

| Flag | Environment | Default |
|---|---|---|
| `-ir-device` | `SLOPIFY_IR_DEVICE` | off |
| `-ir-address` | `SLOPIFY_IR_ADDRESS` | `0x877C` |
| `-ir-volume-up`, `-ir-volume-down` | `SLOPIFY_IR_VOLUME_UP`, `SLOPIFY_IR_VOLUME_DOWN` | `0x88`, `0x8C` |
| `-ir-power-on`, `-ir-power-off` | `SLOPIFY_IR_POWER_ON`, `SLOPIFY_IR_POWER_OFF` | `0x25`, `0xC8` |
| `-ir-sweep` | `SLOPIFY_IR_SWEEP` | `6s` |
| `-ir-idle-off` | `SLOPIFY_IR_IDLE_OFF` | `5m` |
| `-ir-wake` | `SLOPIFY_IR_WAKE` | `3s` |
| `-ir-repeat` | `SLOPIFY_IR_REPEAT` | `110ms` |
| `-ir-initial-steps`, `-ir-max-steps` | `SLOPIFY_IR_INITIAL_STEPS`, `SLOPIFY_IR_MAX_STEPS` | `0`, `60` |
| `-ir-state` | `SLOPIFY_IR_STATE` | `/var/lib/slopify-player/ir-amp.json` |

## Layout

| Path | Contents |
|---|---|
| `.` | Main package and the speaker page in `ui/` |
| `internal/alsa` | Pure-Go PCM and mixer access |
| `internal/audiodec` | Integer FLAC, WAV and MP3 decoders |
| `internal/pulse` | Playback-only Pulse native-protocol server |
| `internal/esound` | Playback-only ESound server |
| `internal/upnp` | UPnP AV renderer protocol |
| `internal/lirc` | IR transmit through the kernel's LIRC device |
| `internal/audiometer` | Peak meters |
| `internal/receiverconfig` | Deployment flags for the sender setup page |
| `deploy/` | Ubuntu install |
| `openwrt/` | OpenWrt image build |
| `shairport/` | Old shairport-sync patches, kept for reference |
