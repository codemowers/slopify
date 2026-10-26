# Slopify

Slopify serves your own music and video collection from a NAS through a web
interface and dedicated Linux audio players. Browse folders or search your
collection, build a queue, and control playback from a phone or desktop.
Your queue follows your signed-in account across browsers.

It is built around ordinary files: embedded tags are optional, filenames fill
metadata gaps, and metadata lives alongside each directory in `.slopify/dir.json`.
The indexer writes these sidecars; it does not delete media files. Legacy
Latin-1 tag bytes are decoded without damaging valid UTF-8 metadata. If tag text
is already corrupted, readable filenames supply the display labels instead.

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

Slopify is under active development. It currently has no ready-made Raspberry Pi
or Cubietruck OS image, offline downloads, cover-art enrichment, Subsonic API
or S3 backend.

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

The Linux player needs `ffmpeg`, `ffprobe`, `aplay` and access to its sound
hardware. It connects outbound to the server over HTTPS; it needs no incoming
ports, discovery or NAS mount. Build the container on the target architecture,
or use an emulating builder:

```sh
docker build -f build/player.Dockerfile -t slopify-player .
docker run -d --restart unless-stopped --read-only --device /dev/snd \
  -v slopify-player:/var/lib/slopify-player \
  -e SLOPIFY_SERVER=https://slopify.example.com \
  -e SLOPIFY_NAME="Living room" \
  -e SLOPIFY_OUTPUT=aplay:hw:0,0 \
  slopify-player
```

Choose the real ALSA hardware device for your DAC. The source sample rate must
be supported by the device. The player uses hardware/OS volume control.
The player root filesystem is read-only; its token volume remains writable.
Keep that volume to preserve the device's identity. After startup, adopt
it in the web app's playback-device settings and select it as an output. The
output selector is at the top right. Offline devices and ordinary browser tabs stay visible when selected and rejoin
the current queue state when they reconnect. Link-based web sinks remain
available regardless of session count.
