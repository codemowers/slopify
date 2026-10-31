# Slopify

Slopify serves your own music and video collection from a NAS through a web
interface and dedicated Linux audio players. Browse folders or search your
collection, build a queue, and control playback from a phone or desktop.
Your queue follows your signed-in account across browsers. The web app’s Slopify
title and the local speaker UI’s footer link to the source repository on GitHub.

It is built around ordinary files: embedded tags are optional, filenames fill
metadata gaps, and metadata lives alongside each directory in `.slopify/dir.json`.
The indexer writes these sidecars; it does not delete media files. Legacy
Latin-1 tag bytes are decoded without damaging valid UTF-8 metadata. If tag text
is already corrupted, readable filenames supply the display labels instead.

Queue rows omit position numbers; folder rows show their name and + action without a redundant “Folder” label.
The queue follows track changes by scrolling the playing item toward the centre, without moving keyboard focus or interrupting manual scrolling on position updates.

## Project goals

1. **First-class web experience.** The browser is the primary client: responsive
   browsing, search, queues and playback on desktop and mobile.
2. **A dedicated local audio player.** A lightweight Linux client fills the
   role of squeezelite: fetch originals, decode locally and drive an ALSA DAC.
   It comes after the web experience in priority.
3. **No backward-compatibility burden.** Use our own APIs and player protocol.
   Legacy client/protocol compatibility is out of scope. Interfaces can change
   when that improves the design.
4. **Performance and security first.** Target responsive use with 100,000+
   files, bounded background work and efficient playback. Use OIDC, scoped
   API authorization, expiring media URLs and restricted filesystem access.
5. **Container-centric deployment.** Build small services as container images
   and deploy them through Kubernetes, Helm and Skaffold. Device OS appliances
   and compatibility with legacy installation layouts are secondary concerns.

## What it does

- Plays music in the browser or on Linux devices connected to an ALSA DAC.
  Devices fetch originals over HTTPS and decode locally, without resampling or
  software volume control.
- Controls one or several outputs from the same queue. Outputs share playback
  commands, but are **not synchronized for multiroom listening**.
- Offers original audio, Opus 128 kbps and MP3 192 kbps in the browser.
- Plays compatible browser video directly. If needed, repackages it as MP4
  without changing codecs, or converts unsupported audio to stereo AAC 192 kbps
  while copying video. The browser must support the source video codec; seeking
  remuxed/converted streams restarts near a keyframe. There is no video transcoding.
- Optionally repeats the queue from its first item when it ends.
- Exports the present queue as `.m3u` or `.pls` for mpv and VLC. Both use
  originals with signed links valid for 12 hours; local playback is independent
  of Slopify's controls.
- Provides OIDC sign-in, device pairing, light/dark themes and optional playback
  diagnostics. Folder additions are capped at 300 tracks; search additions
  are capped at 300 unique tracks.

Video previews sit below the collection/files list on desktop; click the preview
to watch fullscreen. On mobile, the preview sits above playback controls. Queue
rows have individual Play/Remove buttons; tap rows to select several on touchscreens, or
use Ctrl/Cmd-click and Shift-click on desktop to select tracks for dragging or
removal with the Delete key. The × beside each track removes it directly.

Slopify is under active development. It currently has no offline downloads,
cover-art enrichment, Subsonic API or S3 backend.

The proposed [local network audio design](docs/design/local-network-audio.md)
covers Apple, Ubuntu, Android and Windows sources, device authentication,
source switching and pinning, precision checks,
Ubuntu sender setup, a local web UI and metrics. Production receiver admission
and production receiver authentication remain proposed. Device-local source selection is experimental.

## Video playback

Video files live in the same searchable library and per-user queue as music.
Play one in this browser to get a preview below the collection/files panel;
on mobile it appears above the playback controls. Click or tap the preview to
watch fullscreen. The queue's controls handle playback and seeking; native
video controls appear in fullscreen. File tooltips and Nerd mode show source
audio/video codecs and the selected playback path.

Slopify uses the least conversion needed:

| Playback path | Video | Audio |
|---|---|---|
| Original file | Unchanged | Unchanged |
| MP4 remux, if the browser rejects the original container | Copied without re-encoding | Copied without re-encoding |
| Audio conversion, for unsupported codecs such as DTS | Copied without re-encoding | Converted to stereo AAC at 192 kbps |

**Transcoding covers audio only. Video is never transcoded.** Re-encoding
video would require substantially more CPU or dedicated GPU capacity, so
Slopify deliberately leaves the picture untouched. Remuxing changes the
container, not the video codec or quality.

The browser and its hardware must therefore support the original video codec.
Converting DTS to AAC can fix unsupported audio; it cannot make an unsupported
HEVC video stream playable. Original playback uses HTTP range seeking;
remuxed/audio-converted playback restarts at a nearby source keyframe, so
seeking is approximate. Streams that remux video or convert its audio are not cached.

For files your browser cannot play, export the queue as M3U or PLS and open it
in mpv or VLC. Exports always point to the original files, regardless of browser
quality settings. Linux Slopify audio devices can also queue video files, but
extract and play only their audio; they do not display the picture.

## Why another player?

This project grew out of dissatisfaction with the available setups: large
NAS collections, awkward browsing, dated interfaces and playback state tied
to a particular machine. A responsive web UI for **100,000+ files**, usable
from both a phone and desktop, is a core requirement. Folder browsing, useful
filename fallbacks and bounded queue additions matter as much as polished tags.

We do not want to maintain moOde's PHP appliance stack or extend LMS's
Perl-based legacy architecture. Rhythmbox is a desktop player, while
we want a shared library and controller available from any browser. The
existing web interfaces have not met this project's usability expectations;
building a UI we actually want to use is part of the point.

These are our priorities and implementation preferences, not proof that a
language determines performance or that every alternative fails at 100,000
tracks. Slopify has no published comparative benchmark yet; large-library
responsiveness is a design target, not a claimed benchmark victory.

## Browser outputs without sign-in

In playback-device settings, give a new browser output a name and click
**Create link**. Select the named web sink from the signed-in app, even before
a receiver is open. Copy the private link, open it in a receiving browser and
enable playback if prompted. The receiving page needs no account or OIDC
sign-in.

The page shows its assigned queue and plays music/video, but has no library
browser or queue, seek, play/pause or output-selection controls. The signed-in
account still owns the queue. Its identity is the OIDC account, not a mutable
display name.

The same link can be open in multiple browsers at once, following one named
output. They receive the same queue/state but are not sample-synchronized.
A web sink is always available and selectable, even with **0 sessions**. The
UI shows how many sessions are tuned in instead of online/offline status.
Playback continues with no listeners; known track durations advance the queue
on the server, and returning sessions join its current position.

Anyone holding the link can receive that output's playback. **New link**
invalidates the previous link; **Revoke** deletes the output and closes its
receivers. Already issued media URLs expire after five minutes. Links survive
server restarts; the server stores a hash of the secret, so an existing link
cannot be retrieved later—save it or generate a replacement.

## Deploy the server

The supported workflow uses the Helm chart with Skaffold. The chart currently
assumes a Kubernetes cluster with an HTTPS ingress, Passmower's `OIDCClient`
controller and Mittwald's `StringSecret` controller. It is not a standalone
Docker Compose or generic OIDC installation guide.

To install:

1. Have Docker, kubectl, Helm and Skaffold installed, with access to the cluster
   and permission to push images to your registry.
2. Copy [skaffold.env.example](skaffold.env.example) to skaffold.env and
   [the example values](chart/values-local.example.yaml) to
   chart/values-local.yaml. Both local files are gitignored. Set the kube
   context, namespace and image repository in skaffold.env; set allowedUsers to
   the permitted OIDC account IDs and the host media directory in the values.
3. Ensure the media directory exists on the host and UID/GID **1000:1000** can
   write sidecars in its media directories. Catalog and original-file serving
   mount the library read-only. Indexing runs in the background.
4. Ensure the configured StorageClass exists for player state and TopoLVM is
   installed. If the `transcodes` StorageClass does not already exist, create
   it using the command below.
5. Run Skaffold and open your Slopify origin to sign in.

```sh
# Once per cluster, if the transcodes StorageClass is absent:
kubectl apply -f deploy/storageclass-transcodes.yaml

skaffold dev
```

Set `domain` to the public hostname used by the ingress and Passmower
OIDCClient. Services get the canonical public origin from the OIDC_CLIENT_ORIGIN
secret that Passmower generates; API audiences and playlist URLs derive from it. Player queues and device pairings persist on a separate PVC.
Opus and MP3 each have a 25 GiB cache PVC, with a combined 50 GB application
cache budget. Video remux and audio-conversion streams are not cached. Prometheus monitoring
is enabled through an indexer PodMonitor in this installation.

For another installation, start from [chart/values.yaml](chart/values.yaml) and
adapt the Skaffold context, namespace, registry and values file. Configure
`domain`, ingress, media storage, player-state storage and
transcode storage classes. Use `media.hostPath` for a directory on a node, or
`media.existingClaim` for an existing media PVC. A host directory must be
available on whichever node runs the media services.

You can inspect the manifests before deploying; this does not build images:

```sh
helm template slopify chart --namespace music \
  --set domain=music.example.com \
  --set 'allowedUsers[0]=your-account-id' \
  --set imageRepository=registry.example.com/music \
  --set media.existingClaim=music-library \
  --set players.storageClassName=your-state-class \
  --set transcodes.storageClassName=your-cache-class
```

Set `images.<service>` to select published tags or digests, and
`imagePullSecret` for private registries. The chart expects the OIDC and secret
controllers above; using a different identity-provider setup requires adapting
the templates. [deploy/build.sh](deploy/build.sh) is the alternative in-cluster
Kaniko build/deploy workflow for the demo installation.

With network policy enabled, every Slopify workload denies outbound connections
except explicit dependencies. Indexer and original-file serving have no egress;
catalog can reach indexer events; transcoders can fetch originals. Catalog,
players and frontend can resolve names and reach the configured OIDC endpoints.
Set networkPolicy.oidcEgress to the exact ingress peers or destination CIDRs and
ports for your identity provider; there is no general internet/HTTPS exception.

## Connect an audio device

The player runs on Linux under systemd. It connects
outbound to the server over HTTPS; it needs no incoming ports, discovery or NAS
mount. See the [player README](player/README.md) for its goals and status, and the
[Ubuntu 26.04 Raspberry Pi install](player/deploy/README.md).

The device player has two primary goals: **lossless, native-rate playback of
Slopify originals**, and **LAN audio streaming from laptops**. Original playback
must preserve decoded precision; lossless wider-word packing is allowed. Desktop
mixes may already be resampled by their sender, so their format and any receiver
conversion should be visible rather than described as original-file bit-perfect.
See the [device-player goals and architecture](docs/design/local-network-audio.md).

`SLOPIFY_OUTPUT` selects an ALSA `hw:` device, as `alsa:hw:...` (also spelled
`aplay:hw:...`), or `null`. The default engine decodes FLAC, WAV and MP3 in Go with
integer arithmetic and writes directly to the device, with no ffmpeg, mpv or PipeWire
in the path. Originals in other formats, including video containers, play through one
persistent headless mpv process when mpv is installed, and fail visibly otherwise;
`SLOPIFY_MPV` overrides the executable and `-playback-engine=mpv` uses it for everything.
FLAC and WAV output is bit-exact in tests against ffmpeg and through ALSA Loopback. MP3
has no bit-exact definition; its measured error against ffmpeg is far inside the ISO
full-accuracy limit, though MP3 intensity stereo and mixed blocks remain unverified.
The native engine has had little real-world listening. Hardware volume, mute and rotary
controls use the kernel's ALSA control device directly.

With `-local-source-control -pulse-listen=LAN_IP:4713` the player also receives laptops'
Pulse tunnels itself, with a card per sender. There is no authentication: trusted LAN
only. Only the selected source reaches the DAC, at its own rate. Nothing is mixed,
resampled or scaled, and float streams are refused, so a tunnel must send integer samples
(`format=s32le`, which the speaker advertises). Add `-esound-listen=LAN_IP:16001` to also accept ESound clients (8 or 16-bit, mono or
stereo streams only), equally unauthenticated. `-esound-buffer-ms` (default 250) sets its stream buffer, and
`-esound-socket-ms` caps its kernel receive queue, which by default can add about a
second of delay. When enabled, the speaker page shows the registry setting for
the WinESD driver on Windows XP and 2000. `-upnp-listen=LAN_IP:49494` adds an equally unauthenticated UPnP/DLNA renderer, which
answers SSDP on UDP 1900, so a controller such as
[swyh-rs](https://github.com/dheijl/swyh-rs) can send a PC's audio as FLAC, WAV or LPCM.
It only fetches from private addresses. An amplifier with a motorised volume knob and IR input can be driven with
`-ir-device=/dev/lirc0`, using the kernel's `gpio-ir-tx` overlay: see the
[player README](player/README.md). This Go receiver is new. AirPlay is not
available at present.

Add `-local-listen 0.0.0.0:8080` to serve the optional speaker-local UI.
Open `http://SPEAKER_IP:8080/` for live formats, routing, source peak envelopes,
hardware volume in dB and how full each buffer is. Peak charts use a fixed −48 to 0 dBFS range with
12 dB ticks; quieter peaks sit at the bottom without changing measured values.
Source cards show title/artist/album or sender-supplied descriptive media
properties as key: value lines; technical queue fields stay out of that list. Borderless open/closed lock controls pin a source or resume automatic selection;
badges distinguish automatically selected (➜), linked to amplifier (🔊), and
disconnected (🔌). A spinner indicates source changes awaiting an amplifier link. Playback startup
separately reports starting mpv, loading audio and buffering; meter readiness
is independent of these states. Automatic fallback waits while Slopify is
starting/loading/buffering; manual selection still overrides it. The selector contains
Recent and Priority; changing policy while pinned keeps the pin. Priority reorder
arrows sit at the top-right of each source; unavailable directions are hidden. Unlocking in
Recent mode marks the current source as latest, so already-playing peers do not
immediately take over. Explicitly pinning Slopify requests resume only when the
Pi is the queue’s sole selected output; this requires the matching players-service
update. Other selected outputs keep their existing playback state. Laptop tunnels may supply only a stream name.
Slopify startup and buffering indicators appear inside its source card, which omits
the redundant local hostname. PCM labels use a shared renderer: ALSA spellings normalize to abbreviations
such as S32LE, with precision, signedness and byte order in tooltips. Hardware controls live in the amplifier box; there
is no separate Speaker section. Without source control, a standalone Output
section retains playback status and hardware controls.
The slider appears only with hardware volume enabled;
Mute uses the advertised ALSA playback switch and preserves the gain setting.
The page heading and browser title use the local device name. Connection status
is shown only while connecting or reconnecting. The device name defaults to the OS hostname; `SLOPIFY_NAME` overrides it. Setup
commands derive their address from the page URL, with no editable address field.
Pulse and AirPlay/RAOP setup instructions are available; installing tools has a
separate command box. Copy buttons work over ordinary LAN HTTP.

Explicitly pausing Slopify bypasses the silence hold and allows an immediate
automatic fallback to a sounding source; a manual pin still takes precedence.
The fallback countdown has a **Switch now** action that takes the pending
lower-priority source immediately while leaving automatic selection enabled.
A pinned source retains a dashed connection line while disconnected or buffering,
becoming solid when its amplifier link is active. The sender setup uses Zeroconf
native and Fixed tabs without an outer panel. AirPlay appears only with
`-local-airplay` or `SLOPIFY_AIRPLAY_ENABLED=true`, indicating a configured
receiver (not a health check). `SLOPIFY_PULSE_ENABLED` independently controls
Pulse setup visibility. Player defaults are Pulse enabled and AirPlay disabled. mpv cache stalls report
buffering in the source card as well as initial track loading.

Automatic selection keeps the current source playing while Slopify buffers
privately; startup alone never takes priority. Selection waits for fresh sound.
A selected stream has a bounded three-second first-meter grace after playback
readiness, and Priority's silence allowance starts at readiness or the last
audible observation, whichever is later. Explicit pins still select immediately.

Switching sources closes the device and whatever it still held, then the next source
opens it at its own format, so a switch has a short gap.

The local page puts automatic selection below the title divider, without an
enclosing Sources panel. Source and amplifier cards use equal-width desktop
columns and stack on narrow screens. Source charts are compact; the output
chart stays taller. Pending spinners sit at the top right, with explanations
replacing the stream state. Status lines use `2ch · 96kHz · S32LE · running`;
tunnel IPs occupy their own line. The amplifier omits the redundant device name.
The Slopify card title beside its padlock is the configured server hostname,
without URL credentials, path or query; do not repeat it in the details.
Sender instructions include per-user and system-wide persistent Pulse zeroconf
discovery with copy buttons. System-wide configuration applies to each user
session; it does not create a system audio daemon.

Sources and Output share one diagram.
The Pi install advertises its device name as a `_pulse-sink._tcp` mDNS service.
Ubuntu clients can use `pactl load-module module-zeroconf-discover`; the page also
keeps an IP-based `module-tunnel-sink` example for routed/VPN connections where
mDNS does not cross the network boundary. The advertised instance/description
defaults to the hostname; the internal hardware target remains `slopify.hifiberry`.
Discovery does not add authentication to the trusted-LAN Pulse endpoint.

Enable `-local-source-control`, then click a source to select and pin it.
Automatic selection has three modes: Off (pin selected), Recent (last source to
start sounding), and Priority (first sounding source in the configured order).
Drag cards to reorder priorities; arrow buttons provide a keyboard alternative.
Source colours derive from persistent slots. Curved connections show which source
owns the device. Sender IP/title appear when
available, and remembered sources remain visible after disconnecting. This
prototype remembers Slopify and up to seven other sources; unknown extra sources
are blocked. Reconnecting sessions from the same sender use the newest stream;
duplicates do not consume remembered-peer slots. Settings default to `/var/lib/slopify-player/local-audio.json`;
`-local-state` overrides the path, or accepts an empty value for memory only.
The peak threshold is −60 dBFS with a two-second silence hold. Recent mode
requires sustained sound before switching: its Minimum sound duration slider
is 0–2000 ms, defaults to 500 ms, and persists with source settings. It does
not delay higher-priority sources or manual pinning. In Priority mode, the same
slider sets Silence before fallback (0–120 seconds, default 30 seconds), with a
separate saved value. The selected source retains priority through shorter gaps.
A spinner and countdown name the pending fallback only when a sounding
lower-priority source is available; the countdown clears when sound resumes. Grey chart bands indicate transitions,
mixed routing, not measured audio levels. Missing observations leave gaps.
Speaker tabs keep live status in the background and share one event connection
when the browser supports SharedWorker. Pinning a source disables automatic
selection and its timeout slider; unpinning restores the saved automatic mode.
Hardware volume and mute controls appear in the amplifier box when supported. Control requests time out after eight seconds instead of leaving
the UI busy indefinitely; check the current state before retrying a timed-out
command, since it may already have reached the speaker.

Switching away from Slopify requests a pause when this speaker is the queue's
only output. A later explicit Play is respected: playback can run on its private
receiver while the pinned LAN source stays on the amplifier.
Unselected streams keep running at real-time pace, unheard, so switching back is immediate.
When another local source is selected, the player asks the server to pause only
if this device is the queue's sole selected output. With multiple selected
outputs the queue and Slopify stream continue; only the Pi's audible route changes.
The server checks membership and playback revision atomically. Selecting Slopify
again does not automatically resume a paused queue. This requires the updated
players service as well as the updated device player.

This is an unauthenticated trusted-LAN prototype, including volume and source
mutations. Commands require same-origin JSON; leave `-local-listen` unset to
disable the page and source selection.
Sender formats do not establish original recording precision. LAN inventories
and arbitration stay on the device and are not sent to the Slopify server.

A connected laptop's volume slider controls the hardware, within its configured range:
0–100% maps to min–max (0–40 on the Amp2), so 50% is level 20. Changes from the knob or
this page move the slider of the sender connected to the amp, and samples are never
scaled. Mute stays separate. Switching to a sender moves its slider to the current level.

Speaker `/metrics` exports Prometheus histograms of stream and DAC buffer fill, network
and scheduling gaps, plus underflow, underrun and device-open counts. It has no
authentication, like the rest of this page.

Choose the real ALSA hardware device for your DAC. The source sample rate must
be supported by the device. The player uses hardware/OS volume control.
Hardware volume control is opt-in. With no volume flags the player never reads
or changes the mixer. For a HiFiBerry Digital control, add these player arguments:

```sh
-volume-control Digital -volume-card sndrpihifiberry \
  -volume-min 0 -volume-max 40 -volume-initial 20
```

These values use **the 0–100 scale displayed by alsamixer**, with startup at 20
and a cap of 40 in this example. On an Amp2, 20 is about −42 dB and 40 is about
−24 dB; 0 is mute. The initial level is set once each time the player process
starts. The selected device's web control shows
only the hardware dB value (−∞ dB for mute); the slider still commands integer
steps internally. Local mixer changes are checked once per second and published through SSE; both local and web changes
are constrained to the configured range. Volume commands belong to
the account currently selecting the output and do not restart playback.
Rotary actuation runs locally in its own goroutine. Volume telemetry runs in a
separate goroutine, coalescing rapid changes and never blocking local rotation
on HTTP responses. ALSA controls use the kernel's control device directly, without alsa-utils.

For a bare rotary encoder on a Pi with an Amp2, connect channel A to BCM GPIO23
(physical pin 16), channel B to GPIO24 (pin 18), and common to GND (pin 14).
Connect the push switch between GPIO25 (pin 22) and GND. No supply connection
is needed. Configure internal pull-ups and Linux input events in the Pi's
boot configuration:

```ini
dtoverlay=rotary-encoder,pin_a=23,pin_b=24,relative_axis=1,linux_axis=0
dtoverlay=gpio-key,gpio=25,active_low=1,gpio_pull=up,keycode=164,label=SlopifyPlayPause
```

Pass `-rotary-input /dev/input/slopify-rotary` and
`-button-input /dev/input/slopify-button` to the player. Use udev rules matching
input names `rotary@17` and `button@19` (GPIO numbers in hexadecimal) to create
those stable symlinks and grant the player user read access to only these two
devices. Sandboxed systemd services also need `DeviceAllow=char-input r`.
Rotation adjusts the configured mixer by one displayed level per kernel detent
(subject to hardware quantization); click toggles the selected account queue's play/pause state. Releases and repeat events
are ignored and clicks are debounced. Swap A/B if rotation is reversed. Wiring
and boot overlays require a power-off/reboot; the player waits for missing input
devices. GPIO choices must be checked again for other audio boards.

Device seeking uses a temporary loopback range reader: bounded 128 KiB upstream
reads reuse HTTPS connections instead of reopening TLS for every ffmpeg seek.
It caches at most 512 KiB per active seek stream in memory, writes no media to
disk, and closes when that decoder ends. Normal track starts fetch originals
directly. Seeking latency still depends on the source format and network.

The player root filesystem is read-only; its token volume remains writable.
Keep that volume to preserve the device's identity. After startup, choose
**Pair with my account** in the web app's playback-device settings and select it
as an output. Pairing assigns the speaker to your account; other users cannot
select or manage it. Pending devices are visible to signed-in users, and the
first successful claim wins. The default label is `<owner>'s <reported name>`;
Rename sets an independent web label, and clearing it restores the default.
Your device's local name and mDNS identity are unchanged.

Existing adopted devices inherit an owner only if exactly one saved user queue
selects them; other devices require pairing again. Cross-user sharing and
cross-server library/output federation are not implemented; see the
[federation design](docs/design/federation.md), including separate IdPs and
browser playback.

 The
output selector is at the top right. Offline devices and ordinary browser tabs stay visible when selected and rejoin
the current queue state when they reconnect. Link-based web sinks remain
available regardless of session count.

Browser progress updates pause while the page is hidden (including a locked
mobile screen) and refresh when it becomes visible. Track completion and
playback controls still report normally. Mobile OS suspension or a disconnected
event stream can still interrupt playback; suppressing progress requests alone
does not prevent those conditions.
