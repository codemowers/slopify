# Slopify player

The speaker side of Slopify: a single Go binary that drives a DAC over wired Ethernet,
with first-class support for Raspberry Pi 3, Pi 4 and Pi 5.

## Goals

1. **Conversion-free local playback from a Slopify server**, in the role squeezelite
   fills for Squeezebox, using Slopify's own HTTP protocol. The server keeps the queue;
   the player fetches originals and plays them at their native rate and precision.
2. **A PipeWire audio sink for Linux (Ubuntu) users.** A laptop's PipeWire or
   PulseAudio sends its audio to the speaker over a Pulse tunnel.
3. **An audio sink for Mac and iOS users.** AirPlay 1 is implemented in the static
   binary; usability with current macOS and iOS remains to be verified. AirPlay 2
   is deferred until that works.

Ideally all of this is one statically linked binary that runs the same on OpenWrt,
in Docker or anywhere else, over Ethernet.

The local player UI has a deliberately minimal scope: volume, mute and
play/pause, with previous/next planned and possibly selection of an existing
queue later. Catalog browsing, adding tracks and queue editing will never be
part of it; use the primary browser UI on desktop or mobile for those tasks.

The purpose is music playback. Audio-video sync and gaming are not priorities, so sound
quality and reliable playback win over low latency when they conflict.

## Status

| Goal | State |
|---|---|
| Slopify playback | Works. FLAC, WAV, MP3 and mono/stereo Ogg Vorbis are decoded in Go; unsupported formats fail visibly |
| Linux sink | Works on a Raspberry Pi 3 with a HiFiBerry Amp2. New, with little listening time. No authentication: trusted LAN only |
| ESound source | Opt-in with `-esound-listen`. Streams only, no authentication: trusted LAN only. Tried with Windows XP; protocol tests also pass |
| RTP multicast source | Opt-in with `-rtp-receive`: L16 and L24 sessions announced over SAP. Tested and working; parser and stream tests also pass. No authentication: trusted LAN only |
| RTP multicast sender | Opt-in with `-rtp-send`: Slopify audio at its own rate and precision, to a random group unless `-rtp-send-group` is given. Tested and working; packet/report tests also pass. Not encrypted: track titles and audio reach the whole LAN |
| UPnP/DLNA renderer | Opt-in with `-upnp-listen`, for controllers such as swyh-rs. Not yet tried with a real controller. No authentication: trusted LAN only |
| AirPlay 1 (RAOP) source | Advertised as `<speaker name> via AirPlay`, alongside `<speaker name> via PulseAudio` for Pulse. Opt-in with `-raop-listen`: ALAC over UDP, audio, track text and remote control of a selected Apple sender. Works with PipeWire 1.0.5 `module-raop-sink`, unencrypted and RSA. Apple devices need `-raop-key` and are untested. No AirPlay 2 or FairPlay. No authentication: trusted LAN only |
| OpenWrt images | The Raspberry Pi 3 image boots, and plays on a Pi 3 with an Amp2; see [its status](../../appliance/README.md#status). Pi 4/5 release profiles still need hardware playback verification. Cubietruck is a manual experimental target; playback works with the included S/PDIF driver; its current kernel limits S/PDIF to S16_LE |
| Docker image | Builds, but has not played audio |

## Design rationale

**Conversion-free by default.** A Slopify original reaches the DAC at its own sample
rate and full decoded precision. The only changes allowed are lossless: packing 16 or
24 bits into a wider word, or duplicating mono, to suit the hardware. An unsupported
rate is an error, never a silent resample. Sender desktop mixes under goal 2 are a
weaker guarantee, since the sender has already mixed them. The speaker reports what it
received and adds no conversion of its own by default.

The explicit native-ALSA option `-allow-truncate` permits a final 16-bit fallback
when no precision-preserving format works. It discards the low bits without
resampling, rounding or dithering; it applies to queue and LAN audio, including
lossless sources. Successful fallback is logged and the output format reports
S16_LE. The default remains off. The offline `play` command accepts the same flag.
Lossy input labels show decoder precision (for example, `MP3 · 2ch · 44.1kHz · 24-bit decoded`),
so it can be compared with the actual output format. This is not the original recording’s bit depth.

`-cue-level-db` sets the cue peak ceiling in dBFS (−60 to 0, default −6);
the sustained buffering drone is 6 dB lower. OpenWrt uses `slopify.player.cue_level_db`.
Play/pause notes use 440/660 Hz. Initial Play, Resume and selecting another queue track sound an ascending cue, repeating once a second while
startup buffers, until the first decoded audio is ready. The winding-down underrun
tone is suppressed during the first second of decoded music and on cancellation.
Native queue playback gives a quiet descending “bebop” when a playing track is paused.
It plays once, locally, only while Slopify is selected. The player menu’s sound-cue
setting disables it along with source-change, seek and buffering cues.

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
ioctl interface. Decoders and network parsing do not use it. Playback never
launches an external decoder or player.

**In practice:**

- Audio goes straight to an ALSA `hw:` device, with no sound server in between.
- No resampling, mixing, DSP or software volume. FLAC, WAV, MP3 and ALAC
  decoding uses integer arithmetic; Ogg Vorbis deliberately uses floating point.
- Only one source reaches the DAC at a time. The others stay metered and unheard.
- Volume changes the hardware control, when one is configured.

## Build

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
  -o slopify-player ./player
```

Run this from the repository root, because the player shares `internal/httpx` and
`internal/volume` with the servers. The result is static and needs no libasound.

## Checking files offline

The binary can exercise its native decoders directly on a library, with no server:

```sh
# Decode everything as fast as possible; lists only DAMAGED and FAIL files, each damaged
# section with its time, byte offset and length; -v lists clean files too. Exits 1 on any problem.
slopify-player validate -j 4 /media/audio
# Start each file part-way through, as a resumed track does.
slopify-player validate -seek 67 'track.flac'
# Real-time playback, gapless while the format is unchanged.
slopify-player play -output alsa:hw:CARD=sndrpihifiberry,DEV=0 a.flac b.flac
slopify-player play -output alsa:hw:CARD=Dummy,DEV=0 /media/audio/album   # snd-dummy
slopify-player play -output null a.flac                                   # no ALSA
```

Directories are walked for `.flac`, `.wav`, `.mp3`, `.ogg` and `.oga`, skipping dot entries.

## Install

- [Ubuntu 26.04 on a Raspberry Pi](../deploy/README.md), under systemd.
- [OpenWrt images](../../appliance/README.md) for the supported Raspberry Pi 3/4/5 release devices.
  Tag builds use official Image Builder Docker containers without GPIO IR receive.
  Cubietruck and GL-AR750/AR750S remain manual experimental builds, excluded from releases.
- `player/Dockerfile`, built from the repository root. A container needs
  `/dev/snd`, and the input devices if a knob is used.

The player needs `SLOPIFY_SERVER`, then adoption in the web app. With `-local-listen`
it also serves a speaker page and `/metrics` on the LAN, with no login.

Playback position reports acknowledge progress; they do not request seeking.
An explicit seek has its own event identity, entry and target position and is
applied once, even when repeated snapshots carry it. Delayed server updates at
a gapless track boundary therefore do not rewind an already playing track.

Pulse timing replies include output-device delay so a sender can compensate
video timing. The stream's read/write indices already describe its input queue.
The card's A/V offset estimates receiver queue plus device delay; it excludes
the sender, network and any downstream amplifier processing, and does not measure
actual lip-sync error. Live device buffers default to 40–80 ms; ordinary Slopify
playback retains its deeper buffer. Larger explicit live buffers may exceed a
PipeWire tunnel's configured latency budget. A/V support remains experimental.

The native engine downloads the current original to its end in the background, and only
then starts on the next track, so a congested uplink drains a whole track of slack rather
than the ~6 s decoded queue. `-read-ahead-mib` bounds what it holds: -1 (default) is half
of the tighter of RAM and the cgroup memory limit, at most 1024 MiB; 0 fetches only as
decoding needs. A set `GOMEMLIMIT` is raised by the same amount.

Audio metrics carry `stage` (`download`, `input`, `output`), `transport` (`https` for the
queue's originals, `multicast`, `pulse`, `airplay`, `esound`, `upnp`, `cue`) and `source`
labels (the server hostname, or the sender's IP address, `IP@group` for multicast), e.g.
`sum by (stage, transport) (rate(slopify_audio_underruns_total[5m]))`.

## Media controls

`-input` selects an event device or kernel input name; repeat it for multiple
IR receivers, multimedia keyboards and rotary inputs. Each uses fixed media
keys; per-device key remapping is outside the player. `-rotary-input` separately
selects a relative-axis encoder for volume. Standard
Linux Play/Pause, Previous, Next, Mute and Volume ± keys control playback or the
local amplifier; only volume repeats while held. OpenWrt reads standard media keys from every event device and restarts on input
hotplug. Its separate Rotary tab selects relative-axis volume. The IR receive tab persists
remote scancode mappings with per-action Learn buttons, applies them through the
separate `slopify-ir` setup tool and RC-core/`ir-keytable`, and grants
audio-group read access to event devices at player startup. Carrier-frequency fields
are available only when the receiver hardware supports tuning. Official Image Builder
release images lack GPIO IR kernel support: LuCI grays out IR receive, preserves
stored mappings and explains the custom-kernel and receiver-overlay requirements. See the
[OpenWrt setup](../../appliance/README.md#remote-control-input-in-luci).

## Layout

| Path | Contents |
|---|---|
| `.` | Main package and the speaker page in `ui/` |
| `internal/alsa` | Pure-Go PCM and mixer access |
| `internal/audiodec` | Integer FLAC, WAV and MP3 decoders, a float Ogg Vorbis decoder and an ALAC packet decoder |
| `internal/pulse` | Playback-only Pulse native-protocol server |
| `internal/esound` | Playback-only ESound server |
| `internal/upnp` | UPnP AV renderer protocol |
| `internal/rtp` | Multicast RTP receiver with SAP discovery |
| `internal/raop` | AirPlay 1 (RAOP) receiver |
| `internal/audiometer` | Peak meters |
| `internal/receiverconfig` | Deployment flags for the sender setup page |
| `server/deploy/` | Ubuntu install |
| `../../appliance/` | OpenWrt image build |

The local source registry remembers up to 30 LAN sources plus Slopify, including
disconnected senders and their ordering. Remove unused sources in the speaker UI
when the registry is full.

Selected input sources use standard Linux media keys. KEY_MUTE affects only
this output; other selected outputs keep playing. GPIO buttons can choose a
standard keycode through the gpio-key overlay. Mixer mute reports appear in
the web output list. Queue pauses retain the initiating player and event source
for the web UI's pause label and tooltip.

Queue-sharing peers carry bounded topology updates on their existing RTCP groups.
Active peers survive mDNS loss, and known private control endpoints remain polled.
Source changes withdraw queue participation promptly; inactive packet reception
does not count as following. Missing RTCP peers expire after 20 seconds.
Avahi remains available for initial discovery.

## Decoder build options

Native decoders are independently removable at compile time with Go build tags:
`no_flac`, `no_wav`, `no_mp3`, `no_vorbis`, and `no_alac`. For example:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=mips GOMIPS=softfloat \
  go build -tags 'no_vorbis no_alac' -trimpath -ldflags='-s -w' -o slopify-player ./player
```

Ordinary builds include all five. MIPS soft-float builds automatically exclude
Vorbis and its floating-point decoder dependency. ALAC is used by AirPlay;
omitting it prevents ALAC reception. Raw PCM transport is independent of these
tags. Disabled decoders return an explicit unsupported-format error; there is
no external playback fallback.
The OpenWrt wrapper accepts the same tags through `PLAYER_BUILD_TAGS`.

`softfloat` removes the need for an FPU, rather than removing float arithmetic
from the program. Audio sample processing stays integer; timestamps, seeking,
meters, volume curves and metrics still use software floating point where needed.
The MIPS ALSA port uses architecture-correct ioctl directions and native-endian
control values, while PCM sent to the USB DAC remains explicitly little-endian.
