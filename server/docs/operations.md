# Server deployment and behavior

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
2. **A dedicated local audio player with first-class Raspberry Pi 3/4/5 support.**
   A lightweight Linux client fetches originals, decodes locally and drives an
   ALSA DAC. Its local controls stay minimal: volume, mute and play/pause, with
   previous/next planned and possibly selection of an existing queue later.
   Catalog browsing, adding tracks and queue editing belong exclusively in the
   desktop/mobile browser UI and will never be part of the local player UI.
   The player comes after the web experience in priority.
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

The proposed [local network audio design](../../player/docs/design/local-network-audio.md)
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
audio/video codecs and the selected playback path. Nerd mode also lists the
current file's composer, copyright, license, label and ISRC when tagged, and
links MusicBrainz and AcoustID identifiers (as written by taggers such as
Picard) to their pages. Upgrading re-probes every file once to collect them.

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

[compose.yaml](../compose.yaml) is a local-machine example using Linux host
networking and anonymous access to one shared queue. Set `XDG_MUSIC_DIR`,
`XDG_VIDEOS_DIR` and `STREAM_SIGNING_KEY` in a private `.env`, then run
`docker compose --env-file .env -f server/compose.yaml up -d --build`. Media directories must already
exist; only the indexer mounts them writable. The example listens on port 8080
and is intended for a trusted network. Keep its player state separate from
OIDC deployments.

The Kubernetes workflow uses the Helm chart with Skaffold. The chart assumes a
cluster with an HTTPS ingress, Passmower's `OIDCClient` controller and Mittwald's
`StringSecret` controller.

Tagged releases publish the server chart to `oci://ghcr.io/codemowers/charts/slopify`.
Its six server images are pinned by SHA-256 digest; server containers target
Linux AMD64. Install a published version with your private installation values:

```sh
helm upgrade --install slopify oci://ghcr.io/codemowers/charts/slopify \
  --version VERSION --namespace slopify --create-namespace \
  -f chart/values-local.yaml
```

Use the release version without its leading `v` for `VERSION`. GitHub Releases
also include the chart archive, digest-pinned `release-values.yaml`, static Linux
player binaries (AMD64, x86, ARM64, ARMv7, ARMv6 and soft-float MIPS/MIPSLE),
Pi 3/4/5 Image Builder images and `SHA256SUMS`. MIPS builds omit Vorbis.
Pushing a `vMAJOR.MINOR.PATCH` tag runs the release pipeline; a suffix such as
`-rc.1` creates a prerelease. Source-built IR kernels and non-Pi images are excluded.
Pushes to `main` run the same builds and chart packaging, retaining
Actions artifacts without publishing images, the OCI chart or a GitHub Release.

To install:

1. Have Docker, kubectl, Helm and Skaffold installed, with access to the cluster
   and permission to push images to your registry.
2. Copy [skaffold.env.example](../../skaffold.env.example) to skaffold.env and
   [the example values](../../chart/values-local.example.yaml) to
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
kubectl apply -f server/deploy/storageclass-transcodes.yaml

skaffold dev
```

Set `domain` to the public hostname used by the ingress and Passmower
OIDCClient. Services get the canonical public origin from the OIDC_CLIENT_ORIGIN
secret that Passmower generates; API audiences and playlist URLs derive from it. Player queues and device pairings persist on a separate PVC.
Opus and MP3 each have a 25 GiB cache PVC, with a combined 50 GB application
cache budget. Video remux and audio-conversion streams are not cached. Prometheus monitoring
is enabled through an indexer PodMonitor in this installation.

For another installation, start from [chart/values.yaml](../../chart/values.yaml) and
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
the templates. [server/deploy/build.sh](../deploy/build.sh) is the alternative in-cluster
Kaniko server/build/deploy workflow for the demo installation.

### Public demo

`server/deploy/demo.sh` runs `skaffold run -p demo` against the public demo on
`ee-lte-1.codemowers.io`, namespace `demo`, using that cluster's development
registry and x86 nodes. It sets the target environment explicitly without modifying your
private `skaffold.env`. Copy [the installation values example](../../chart/values-local.example.yaml)
to `chart/values-demo.yaml` first. The profile loads `chart/demo.yaml` plus these
local values; it never merges the private installation's values.

The demo keeps OIDC login and separate user queues, but `allowAllUsers: true`
removes the account allowlist. Ordinary deployments still require `allowedUsers`.
Services mounting persistent media/state use single-replica StatefulSets to satisfy
this cluster's storage policy; transcode disk caching is disabled in the demo.

The seeded selection includes Kimiko Ishizaka's complete Open Goldberg Variations
in FLAC, Beethoven's Moonlight Sonata, Bach cello, Dr.Esc's six-track Estonian
release *Mulle meeldib rongiga sõita*, and 1000 Handz's DnB track *Long Distance*.
The demo menu links to artist credits, sources and recording licenses.

An init container in the indexer Pod downloads the recordings listed in
[the seed manifest](../cmd/demoseed/tracks.json). It checks each file's size and
checksum, writes hidden partial files, and atomically publishes only complete downloads without overwriting files.
Reruns verify and skip existing recordings; different existing media is never
replaced. Downloads happen on the cluster, and the demo indexer Pod gets public HTTPS egress for initialization. The indexer
starts after import/verification, then uses its ordinary initial scan and inotify;
there is no periodic rescan. The media and player-state PVCs survive redeployment. The demo uses
`/data/demo-players.json`, leaving incompatible state from the old installation
untouched.

With network policy enabled, every Slopify workload denies outbound connections
except explicit dependencies. Outside demo initialization, indexer and original-file
serving need no egress;
catalog can reach indexer events; transcoders can fetch originals. Catalog,
players and frontend can resolve names and reach the configured OIDC endpoints.
Set networkPolicy.oidcEgress to the exact ingress peers or destination CIDRs and
ports for your identity provider. The demo importer's HTTPS permission applies only
to the demo indexer Pod; ordinary deployments have no general HTTPS exception.

## Connect an audio device

The player runs on Linux, under systemd on Ubuntu or procd on OpenWrt. It connects
outbound to the server over HTTPS. Playing the Slopify queue needs no incoming ports,
discovery or NAS mount; only the opt-in speaker page and LAN receivers listen. See the
[player README](../../player/README.md) for its goals and status, the
[Ubuntu 26.04 Raspberry Pi install](../../player/deploy/README.md) and the
[OpenWrt images](../../appliance/README.md). Supported release devices are Raspberry Pi 3/4/5. Tag builds produce their images
with the official OpenWrt Image Builder Docker containers and static Go binaries.
GPIO IR is unavailable in those stock kernels; LuCI grays out IR receive settings
and explains the required kernel, receiver overlay and setup steps. The
[Amp4 IR notes](../../appliance/README.md#how-the-hifiberry-amp4-setup-gained-ir)
document the separate experimental kernel work and LuCI button learning.
Source-built kernels, Cubietruck and AR750/AR750S images are excluded from release
CI. The OpenWrt guide includes manual experimental Cubietruck and AR750S builds.

The device player has two primary goals: **lossless, native-rate playback of
Slopify originals**, and **LAN audio streaming from laptops**. Original playback
must preserve decoded precision; lossless wider-word packing is allowed. Desktop
mixes may already be resampled by their sender, so their format and any receiver
conversion should be visible rather than described as original-file bit-perfect.
See the [local network audio design](../../player/docs/design/local-network-audio.md).

`SLOPIFY_OUTPUT` selects an ALSA `hw:` device, as `alsa:hw:...` (also spelled
`aplay:hw:...`), or `null`. The player decodes FLAC, WAV and MP3 in Go with
integer arithmetic, and mono/stereo Ogg Vorbis in floating point, and writes directly to the device, with no ffmpeg, mpv or PipeWire
in the path. Unsupported formats, including audio in video containers, fail visibly.
FLAC and WAV output is bit-exact in tests against ffmpeg and through ALSA Loopback. MP3
has no bit-exact definition; its measured error against ffmpeg is far inside the ISO
full-accuracy limit, though MP3 intensity stereo and mixed blocks remain unverified.
MP3 uses precomputed requantization tables and symmetric integer transforms to
reduce startup and decoding work on small CPUs.
The native engine has had little real-world listening. Hardware volume, mute and rotary
controls use the kernel's ALSA control device directly.

With `-local-source-control -pulse-listen=LAN_IP:4713` the player also receives laptops'
Pulse tunnels itself, with a card per sender. There is no authentication: trusted LAN
only. Only the selected source reaches the DAC, at its own rate. Nothing is mixed,
resampled or scaled. The speaker advertises `format=s32le`; float32 streams, such as
Chrome's, are unpacked with integer operations, and samples beyond full scale are cut off
and counted. Add `-esound-listen=LAN_IP:16001` to also accept ESound clients (8 or 16-bit, mono or
stereo streams only), equally unauthenticated. `-esound-buffer-ms` (default 250) sets its stream buffer, and
`-esound-socket-ms` caps its kernel receive queue, which by default can add about a
second of delay. When enabled, the speaker page shows the registry setting for
the WinESD driver on Windows XP and 2000. `-upnp-listen=LAN_IP:49494` adds an equally unauthenticated UPnP/DLNA renderer, which
answers SSDP on UDP 1900, so a controller such as
[swyh-rs](https://github.com/dheijl/swyh-rs) can send a PC's audio as FLAC, WAV or LPCM.
It only fetches from private addresses. `-rtp-receive` adds an equally unauthenticated receiver for
multicast RTP announced over SAP, such as `pactl load-module module-rtp-send source=SINK.monitor`
from PulseAudio or PipeWire: uncompressed L16 and L24 only, with one card for each sender on each group.
Multicast cannot be slowed down and nothing is resampled, so clock drift occasionally makes the
audio jump, and lost packets play as silence. RTP multicast reception and transmission are tested and working. `-rtp-send` does the opposite for Slopify playback: it multicasts the
decoded audio, with track titles, to a random group at each start, or to `-rtp-send-group`. L16 and L24 are standard; sources
with more precision, including MP3, go as L32, which only `-rtp-receive` understands. Nothing is encrypted, so anyone on the LAN can listen. The speaker page shows
the group, format and queue beside the hardware output.

`-raop-listen :5000` adds an AirPlay 1 (RAOP) receiver, advertised over mDNS:
ALAC over RTP/UDP, audio only, no authentication. PipeWire and PulseAudio send it
unencrypted. iPhone, iPad and Mac need RSA encryption, enabled by `-raop-key` with
the RAOP private key, which Slopify does not ship: the OpenWrt image downloads it
from the shairport-sync repository when AirPlay encryption is enabled, and the
player accepts only the genuine key. Track titles appear on the source card, and
while an Apple sender is selected the speaker's play/pause, next and previous
controls drive it. Usability with current macOS and iOS remains to be verified.
AirPlay 2 is deferred until AirPlay 1 works with those clients; its additional
complexity is outside the current scope. Chromecast will not be implemented
because of its receiver authentication requirements. Spotify remains unimplemented;
track the ongoing upstream playback failure in
[librespot #1649: Audio key error](https://github.com/librespot-org/librespot/issues/1649).
LAN authentication is deferred; receivers still require a trusted LAN.

Add `-local-listen 0.0.0.0:8080` to serve the optional speaker-local UI.
Open `http://SPEAKER_IP:8080/` for live formats, routing, source peak envelopes,
hardware volume in dB and how full each buffer is. Peak charts use a fixed −48 to 0 dBFS range with
12 dB ticks; quieter peaks sit at the bottom without changing measured values.
Source cards show title/artist/album or sender-supplied descriptive media
properties as key: value lines; technical queue fields stay out of that list. Borderless open/closed lock controls pin a source or resume automatic selection;
badges distinguish automatically selected (➜), linked to amplifier (🔊), and
disconnected (🔌). A spinner indicates source changes awaiting an amplifier link. Playback startup
reports opening the output, loading audio and buffering; meter readiness
is independent of these states. Automatic fallback waits while Slopify is
starting/loading/buffering; manual selection still overrides it. The selector contains
Recent and Priority, and a pin turns automatic selection off; changing policy while pinned keeps the pin. Priority reorder
arrows sit at the top-right of each source; unavailable directions are hidden. Unlocking in
Recent mode marks the current source as latest, so already-playing peers do not
immediately take over. Explicitly pinning Slopify requests resume only when the
Pi is the queue’s sole selected output. Other selected outputs keep their existing playback state. Laptop tunnels may supply only a stream name.
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
becoming solid when its amplifier link is active. Its card explains whether the
queue is paused or it is waiting for audio/a connection, and states that a pin
disables automatic switching. Other connected sources show “Available · not selected”.
The sender setup uses Zeroconf
native and Fixed tabs without an outer panel. AirPlay appears when `-raop-listen`
is set, and notes when encryption, which Apple devices need, is off.
`SLOPIFY_PULSE_ENABLED` independently controls Pulse setup visibility. Player
defaults are Pulse enabled and AirPlay disabled. Buffering and initial track
loading appear in the source card.

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
The player advertises its Pulse receiver as a `_pulse-sink._tcp` mDNS service.
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
prototype remembers Slopify and up to 30 LAN sources; unknown extra sources
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
The local page shows the board model beneath its configured name. USB outputs
use their manufacturer/product descriptors; other outputs retain the ALSA PCM name.
Speaker tabs keep live status in the background with a direct SSE connection per
tab. The speaker keeps the newest three streams and closes the oldest when a
fourth opens; an evicted tab needs a reload to reconnect. Chart history is sent once on connection,
then only new points. Source and output charts receive every measured window,
batched independently of the status update interval. Chart levels use whole dBFS integers; this display
quantization does not alter audio. Metering takes the maximum absolute decoded
PCM value in each window; source selection compares integer magnitudes directly.
The Slopify card's input bitrate is encoded bytes per decoded audio second,
calculated in the browser from SSE counters. MP3 frame accounting excludes tags
and prefetch; other native originals use reader bytes, so startup buffering can
affect short estimates. This is not download throughput or decoded PCM bitrate.
Unchanged status fields are latched in the
browser and sent only when they change. `-local-sse-interval=1s` reduces status update work
on small CPUs (default 200ms, range 100ms–10s); audio metering keeps its 200ms windows.
OpenWrt exposes the interval as `slopify.player.local_sse_interval_ms`.
`-metrics=false` disables Prometheus updates and `/metrics`, including TCP
sampling; metrics default off on MIPS and on elsewhere. Use `-metrics=true`
to opt in on a router, or OpenWrt's `slopify.player.metrics` (`0`/`1`, empty for automatic).
Pinning a source disables automatic
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
The server checks membership and playback revision atomically. Automatically
selecting Slopify again never resumes a paused queue; explicitly pinning it does,
under the same sole-output check. If detached after output expiry, an explicit Slopify pin rejoins only this speaker to its owner’s queue, preserving other outputs and ongoing playback, just like the physical play/pause button. The card shows “Not joined to queue” instead of a stale paused state.

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
be supported by the device. Volume only ever changes the hardware mixer.
Hardware volume control is opt-in. With no volume flags the player never reads
or changes the mixer. For a HiFiBerry Digital control, add these player arguments:

```sh
-volume-control Digital -volume-card sndrpihifiberry \
  -volume-min 0 -volume-max 40 -volume-initial 10
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

Pass `-input /dev/input/slopify-rotary` and
`-input /dev/input/slopify-button` to the player, or the kernel names
`rotary@17` and `button@19` directly. On Ubuntu, use udev rules matching
input names `rotary@17` and `button@19` (GPIO numbers in hexadecimal) to create
those stable symlinks and grant the player user read access to only these two
devices. Sandboxed systemd services also need `DeviceAllow=char-input r`.
Rotation adjusts the configured mixer by one displayed level per kernel detent
(subject to hardware quantization); click toggles the selected account queue's play/pause state. Releases and repeat events
are ignored and clicks are debounced. Swap A/B if rotation is reversed. Wiring
and boot overlays require a power-off/reboot; the player waits for missing input
devices. GPIO choices must be checked again for other audio boards.

The native engine reads originals through bounded range requests and reuses HTTPS
connections while seeking. `-read-ahead-mib` bounds compressed download buffering
separately from the fixed 2 MiB decoded PCM queue (about 5.9 seconds at stereo
44.1 kHz). Short codec reads are combined into full PCM blocks, so MP3 can use
that capacity. It writes no media to disk. Seeking latency still depends on the
source format and network.

Keep the token file (`SLOPIFY_TOKEN_FILE`) to preserve the device's identity. After startup, choose
**Pair with my account** in the web app's playback-device settings and select it
as an output. Pairing assigns the speaker to your account; other users cannot
select or manage it. Pending devices are visible to signed-in users, and the
first successful claim wins. Speakers use their configured local name in the web
app, without an owner prefix or separate web rename.

Paused queues show who paused them; the tooltip identifies the event source and
time. Input sources use standard Linux media keys: KEY_MUTE affects only
that output, leaving the shared queue playing on other speakers. A GPIO button
can emit KEY_MUTE (113) through its gpio-key overlay. Hardware mute status
appears in the web output list.

Existing adopted devices inherit an owner only if exactly one saved user queue
selects them; other devices require pairing again. Cross-user sharing and
cross-server library/output federation are not implemented. Cross-user output
sharing is planned through federation, not LAN takeover; see the
[federation design](design/federation.md), including separate IdPs and
browser playback.

The output selector is at the top right. Offline devices and browsers stay visible when selected and rejoin
the current queue state when they reconnect. Link-based web sinks remain
available regardless of session count.

Each browser appears as one output. The most recently opened tab plays it; older
tabs offer “Use here”, and pressing Play in a tab moves the browser's playback
there. While music plays, the old tab keeps playing until the new one has
started, so a tab the browser will not let autoplay hands back instead. To keep
another screen playing on purpose, open a web sink link there.

Selected outputs stay selected only for a while after playback stops: by
default 30 minutes, adjustable per output (2 or 8 hours, or always) under
“Keep selected when stopped”. After that they are released, and Play in a tab
plays only there, so a speaker in a bedroom is not woken by a later Play.
Pressing play on a speaker itself selects it again.

Device players have a 30-second server-side reconnect grace: a dropped event
connection alone does not pause buffered playback or displace the selected primary.
Reconnecting cancels the timeout. Explicit pause and source-selection commands
still apply immediately, including the existing sole-output check.

Browser progress updates pause while the page is hidden (including a locked
mobile screen) and refresh when it becomes visible. Track completion and
playback controls still report normally. Mobile OS suspension or a disconnected
event stream can still interrupt playback; suppressing progress requests alone
does not prevent those conditions. Installed as an app from the home screen,
Slopify keeps playing through a locked screen even when its event connection
drops: the server keeps it selected for the current track and learns the next
one from the app's own report. An app the OS stops altogether still pauses.

OpenWrt Raspberry Pi images expose EEPROM-based Auto, Off and installed audio
HAT overrides in LuCI; overlay changes require a reboot. See the
[OpenWrt guide](../../appliance/README.md) for board selection and laptop multicast
sending commands. Laptop Pulse outputs include “via PulseAudio” in their names
to distinguish them from other receiver protocols.

The speaker page’s sender setup separates PulseAudio Zeroconf, PulseAudio Fixed,
and enabled AirPlay, ESound and Multicast RTP receivers into tabs. The Multicast
RTP tab provides one repeatable per-user PipeWire-Pulse startup snippet and
copyable removal commands. The RTP and SAP modules are not bound to an
interface, so they follow the current route across docking and network changes,
and are marked `nofail` so a multicast failure never stops PipeWire-Pulse. The laptop output is named Multicast RTP [group address]
because every listening speaker can receive the stream. Persistent laptop setup
derives the audio group from a domain-prefixed hash of `/etc/machine-id` and
announces it on the shared SAP group. OpenWrt saves a generated group in UCI
and exposes an override in LuCI. Different sender IPs sharing a group remain
separate selectable sources; identical machine IDs or hash collisions do not
merge those cards. Source preferences still use sender IP plus group, so a DHCP
address change can create a new source.

OpenWrt SSH logins show Slopify ASCII art instead of the stock banner and package-manager help, with player status and local setup links.
Laptop unicast and multicast configurations coexist in separate PipeWire drop-ins.
The multicast output locks volume and mute at unity; use each speaker’s volume
control (application volume controls still apply).

The local speaker page keeps its speaker name visible at the top while scrolling.

The local multicast sender offers `/multicast.sdp`, an SDP snapshot of its current
announced format, plus VLC/mpv commands and M3U/PLS downloads referencing that URL.
No SDP is available before first playback. Reopen after format changes; downloaded
SDP files do not update. VLC requires its Live555 module and supports L16/L24,
not the private L32 extension.
Windows XP SP3 can use 32-bit VLC 3; bundled Windows Media Player cannot receive
this PCM/RTP stream.

The speaker diagram lists multicast receivers reporting over RTCP, with one line
from Slopify to each receiver showing recent packet reception. Idle or stale
receivers have no connection line. Cards show name, IP, last report, interval loss and jitter. Reporting
uses the RTP port plus one; receivers without RTCP reports are not listed, and
reports do not establish audible playback. The inventory is capped at 32 and
expires after 90 seconds without reports. When local playback stops, pauses or
seeks, or multicast is muted, Slopify sends an RTCP BYE, and a Slopify receiver ends
that stream at once (logging `rtp bye received`) rather than counting the silence
as an underrun. Senders that do not send BYE still end after 3 seconds of silence. It stays on the local speaker UI.
Multicast copies Slopify playback independently of hardware source selection.

The Slopify source hostname links to the configured server origin in a new tab.
Drag or arrow-button source ordering works in Off, Recent and Priority modes;
only Priority uses that order for automatic precedence. Startup hardware volume
defaults to 10 on the alsamixer displayed scale, with the existing 0–40 bounds.


Players with the local web UI enabled advertise `_slopify._tcp` through Avahi.
The title's optional settings cogwheel uses `-local-settings-url` (a relative
path or HTTP(S) URL) and `-local-settings-port` (optional port override). OpenWrt
links directly to `/cgi-bin/luci/admin/services/slopify` on LuCI's configured HTTP
port, preserving the hostname used to open the player.
Discovered siblings appear below the title as links opening in new tabs, using
advertised `.local` hostnames and HTTP ports. Discovery works while idle, excludes
the current instance, and requires the Avahi D-Bus service. It does
not exchange queue identity, media URLs or credentials, or elect playback leaders.

OpenWrt uses port **80 for the player** and **8080 for LuCI**. Existing stock
80/8080 configurations migrate once; custom bindings and LuCI HTTPS remain intact.
The player still runs as nobody:audio, with only CAP_NET_BIND_SERVICE added.


Priority routing has a separate **Higher-priority takeover delay** (0–10 seconds,
default 0). A candidate must remain continuously audible for that duration;
brief sounds and gaps reset the guard. Lower-priority fallback still uses its
own silence allowance, and manual pins bypass the delay.

**Play a quiet tone when switching sources** enables an original 220 ms ascending
cue, on by default and persisted in local source settings. It is synthesized
locally with integer arithmetic, respects hardware volume/mute, and finishes
before the next source opens. It is never mixed into music, multicast or source
meters, and does not repeat at track changes or ordinary reconnects.

Optional device queue deduplication (`-queue-share`, off by default) reuses one
player's multicast audio when multiple LAN players select the same Slopify queue.
It requires the native engine, local source control and RTP reception. OpenWrt's
`slopify.player.queue_share` persists this setting. The server supplies a random,
persisted queue identity and a command tag, but receives no LAN membership or
election state. Devices exchange bounded opaque tags and playback progress over
their existing trusted-LAN HTTP listeners; signed URLs never leave their player.
Each device keeps its cloud control connection, while followers stop their own
original download/decoder. Explicit multicast senders win election first, then
process IDs break ties. Switching away from Slopify withdraws eligibility even
if the old sender continues transmitting. Missing peer/audio updates fall back
to direct playback, normally within a few seconds; transitions can have gaps.
A single listener uses direct playback (an explicitly enabled RTP sender stays
on while local Slopify is selected). This is shared playback, not sample-synchronized multiroom audio.

Automatic multicast input cards are shown at 20% opacity and the Slopify card
names the upstream device. Pinning that input makes it fully opaque and selects
it directly, independent of queue following. A solid source-coloured line at 20% opacity connects the multicast input
directly to the amplifier, without a duplicate Slopify-to-amplifier line. A follower relays matching
progress/end telemetry through its own existing device credentials, so the
server's primary-output rule still advances the queue. Discovery and multicast
remain unauthenticated trusted-LAN features.

The amplifier dBFS timeline records the actual multicast input colour while
following, retaining that colour in history after later source changes.

Pinned Slopify multicast listeners advertise their chosen sender instance/group,
including on receiver-only devices. This demand keeps an implicit sender running
with no other automatic queue listeners; that sender retains its own decoder
instead of becoming a follower. Duplicate peer records count once, and one missed
poll does not bypass the three-second peer grace period. Internal leader changes
are silent; transition cues apply only to changes of the selected logical source.
The Slopify card summarizes Direct, Sending to N players, or Following NAME, with
fallback reasons. Idle multicast output panels are hidden unless there is an
error. Disconnected unselected sources collapse their charts and details, with an
accessible Details button. Pin labels distinguish following the queue from
staying with a particular multicast sender.

Avahi peer resolution preserves the discovery record’s address family and
prefers IPv4 endpoints for matching IPv4 RTP streams on dual-stack networks.

The player header's hamburger holds routing policy, sound/takeover/fallback
sliders, the transition-sound toggle and the relative LuCI “Player settings…”
link. The toggle also covers a short sweep on seeking and a looping winding-down
sound that fills the device when downloading or decoding stalls mid-track. Diagnostics remain beside their sources/output. Escape or clicking outside
closes the menu. Natural forward track completion preserves QueueCommand and
existing multicast followers; explicit commands and repeat wraps invalidate it.
Election/membership use queue identity and continuity command rather than a
transient current-track match. New followers still require matching current
track/audio; existing followers keep their route through a bounded three-second
RTP format-change gap. The physical multicast-to-output line remains through
that interval, without a duplicate Slopify-to-output line or decoder restart.

Direct multicast pins also match the saved sender IPv4 address and multicast
group, so sender restarts do not require audio to start before listener demand
can be recognized. Different sender IPs on the same group remain distinct.

Multicast transmission and SAP advertisement require an eligible locally decoded
source to be selected. Currently only direct Slopify playback is eligible;
PulseAudio, ESound, incoming multicast, UPnP and AirPlay are not. Automatic
multicast-backed Slopify is also ineligible. Explicit sender configuration and
pinned downstream demand cannot override this source gate. Switching to an
ineligible source clears queued RTP audio, withdraws SAP, hides SDP and suppresses
sender reports; returning to eligible local playback restores advertisement.
Only Slopify and the planned Spotify backend (source ID `spotify`) are allowlisted; that
backend must still feed the sender itself. AirPlay and other LAN inputs are never
retransmitted. Source eligibility is separate from election.

Queue election prefers eligible senders required by direct multicast pins, then
explicit sender configuration, then deterministic identity. This avoids a
preferred sender competing with a source that must stay live for pinned listeners.
A fresh sibling report that it is receiving or otherwise ineligible hides its
unselected RTP card and excludes it from automatic selection. Existing pins and
remembered source order are preserved; the card returns when sending is eligible.

OpenWrt LuCI exposes Pulse, ESound and multicast RTP input buffer settings, plus
the ESound socket receive queue and the shared ALSA output buffer. Empty fields
retain protocol defaults; Save & Apply restarts the player. Multicast RTP sends
and receives uncompressed PCM (L16/L24). Images include `kmod-tcp-bbr`, whose
module autoload and sysctl settings select BBR persistently for TCP connections;
this does not change UDP multicast transport.

The native GNOME Shell extension in `desktop/` uses GJS, Avahi D-Bus
and MPRIS, with no Python service or build step. Its dropdown contains metadata,
hardware volume slider, transport and source/player selection; native OSD names
the speaker and dB. Media keys are captured only in desktop/overview modes, with
persisted backup and restore on disable or Laptop controls. Local
`GET/POST /api/playback` exposes bounded queue metadata and version/entry-checked
transport through the controller owner, retaining same-origin JSON checks and
device-scoped cloud authorization without retries. See [the extension guide](../../desktop/README.md).

GL.iNet GL-AR750 and GL-AR750S NOR/NAND retain manual experimental [OpenWrt player build profiles](../../appliance/README.md#glinet-gl-ar750-and-gl-ar750s).
They use a USB DAC, a MIPS soft-float player with Vorbis omitted, and all wired
ports as an L2 LAN switch with DHCP client addressing. Wi-Fi client setup remains
available in LuCI. Native codecs have [individual build tags](../../player/README.md#decoder-build-options).
These router profiles are not supported release devices, are excluded from CI
artifacts, and are not yet verified on physical hardware.
