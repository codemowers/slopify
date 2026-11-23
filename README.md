# Slopify

Slopify is a self-hosted music and video library with a browser player and
controller for desktop and mobile. A dedicated Linux audio player turns a
Raspberry Pi or another ALSA device into a network speaker.

## Goals

- Make a large personal library easy to browse, search and play from any browser.
  Responsiveness with 100,000+ files is a target, not a published benchmark.
- Keep queues on the server so browsers and speakers can share playback.
- Play music originals at their own sample rate and decoded precision, without
  silently resampling or reducing precision.
- Offer first-class Raspberry Pi 3/4/5 players and optional laptop audio reception.
- Deploy small services in containers with Helm and Skaffold.

## Features

- Folder browsing, search, metadata, queues and output selection in the browser.
- Browser music and video playback, signed original downloads and optional
  server transcoding for browser-compatible formats.
- Private browser-output links that let a receiver play without signing in.
- Linux speakers with direct ALSA playback, hardware volume and mute, physical
  media keys and an optional local speaker page.
- Laptop audio through PulseAudio/PipeWire and multicast RTP; opt-in AirPlay 1,
  ESound and UPnP reception. Receiver maturity varies; see the player guide.
- A GNOME desktop remote and Raspberry Pi OpenWrt images.

The main browser UI owns catalog browsing and queue editing. The local speaker
UI stays focused on volume, mute, playback and input selection.

## Get started

- [Server](server/README.md): deploy the library and browser UI.
- [Player](player/README.md): connect a Linux audio output.
- [Appliance](appliance/README.md): Raspberry Pi 3/4/5 OpenWrt images.
- [Desktop](desktop/README.md): control speakers from GNOME.

Version tags publish a GitHub Release with player binaries, Pi images, checksums
and a Helm chart. Server images and the digest-pinned chart are published to GHCR.
Release notes list the downloads, server image digests and Helm install command.
Reruns upload missing attachments only; differing attachments require a new tag.
Pushes to `main` build and package these artifacts without publishing them.

## Documentation

Component guides in `server/docs/`, `player/docs/`, `desktop/docs/` and
`appliance/docs/` cover deployment, architecture, receiver setup and limitations.
The [Amp2 rotary example](appliance/docs/hifiberry-amp2/README.md)
and [Amp4 IR receiver example](appliance/docs/hifiberry-amp4/README.md) describe two speaker setups.

The repository has separate `server/`, `player/`, `desktop/` and `appliance/`
components, with one Go module and shared HTTP and volume helpers in `internal/`.
