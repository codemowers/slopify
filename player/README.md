# Slopify player

A Linux audio player that turns an ALSA device into a Slopify network speaker.
It connects to the server over HTTPS and plays the selected queue directly.
Raspberry Pi 3/4/5 are the first-class appliance targets.

## Goals

1. Play Slopify originals at their own sample rate and full decoded precision.
2. Receive laptop audio over the LAN with a simple, reliable speaker setup.
3. Keep local controls small: volume, mute and play/pause; previous/next are
   planned, with existing-queue selection a possibility later.

Catalog browsing, adding tracks and queue editing belong in the desktop/mobile
browser UI and will never be added to the local player UI. Music quality and
reliability take priority over gaming latency or audio-video synchronization.

## Features

- Direct ALSA playback without a sound server or external decoder process.
- FLAC, WAV, MP3 and mono/stereo Ogg Vorbis decoding; unsupported formats fail
  visibly. Soft-float MIPS builds omit Vorbis.
- Hardware volume and mute, Linux media keys and optional rotary volume input.
- Optional trusted-LAN speaker page, source selection and status charts.
- Experimental Pulse A/V latency reporting, with estimated receiver offset shown
  in the speaker page. Lip-sync accuracy still needs testing.
- PulseAudio/PipeWire laptop reception and tested multicast RTP reception/sending.
- Opt-in ESound (tried with Windows XP), UPnP and AirPlay 1 reception.
  AirPlay with current Apple clients and UPnP with real controllers still need
  verification; AirPlay 2 is not implemented.
- Offline `validate` and `play` commands for checking and playing local files.

LAN receivers and the speaker page have no login and are intended for a trusted
network. The browser UI remains the full library controller.

## Install and use

Use a player binary from GitHub Releases or build from the repository root:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/slopify-player ./player
bin/slopify-player -help
```

Set `SLOPIFY_SERVER` and `SLOPIFY_OUTPUT` to your server origin and ALSA `hw:`
device, start the player, and adopt it in the browser UI. For an installed service,
see the [Linux installation guide](docs/linux-install.md). For a complete
Pi image, see [Appliance](../appliance/README.md).

`-output-buffer-ms` sets the ALSA output buffer for all sources, including
standalone `play`, from 40 to 10000 ms (10 seconds). Zero keeps automatic defaults:
40–80 ms for LAN sources and 200–500 ms for Slopify originals. Actual capacity
depends on the sound card; larger buffers add playback delay. Input and download
buffers are separate. OpenWrt exposes the setting in LuCI's General tab.

The [player guide](docs/implementation.md) covers command-line options,
receiver status, decoder choices, format guarantees and limitations. Hardware
examples live under `appliance/docs/`: [Amp2 rotary](../appliance/docs/hifiberry-amp2/README.md) and
[Amp4 IR receiver](../appliance/docs/hifiberry-amp4/README.md).
