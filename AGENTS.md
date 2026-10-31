# Agent guide

Keep this file for implementation decisions, repository navigation and working
context. README.md is for people evaluating, deploying and using Slopify.
Update both when behavior changes; do not append task transcripts to either.
User instructions take precedence over these defaults.

## Product priorities

- First-class browser usage is the primary product: desktop/mobile library,
  search, queues and playback. The dedicated local audio player is second,
  filling a squeezelite-like role with our own HTTP protocol.
- No backward compatibility requirement. Do not add MPD, SlimProto/Squeezebox
  or other legacy protocol adapters merely to preserve an existing ecosystem.
  Prefer a better API/design over retaining obsolete interfaces.
- Prioritize performance and security in design decisions. Large-library
  responsiveness (100,000+ files) is a target, not an established benchmark.
  Bound work and memory, preserve authorization and filesystem boundaries,
  and avoid trading security for convenience.
- Container-centric architecture and deployment: small images/services,
  Kubernetes, Helm and Skaffold. A standalone player OS is secondary.
- Keep README claims aligned with implemented behavior. Distinguish product
  goals and subjective comparisons from measured performance and upstream facts.

## Working conventions

- Use Go for services and tools. Browser code is plain JavaScript/CSS/HTML in
  `web/`, embedded by `cmd/frontend`. Do not introduce a frontend framework,
  broker or CGO/libav dependency without an explicit change of direction.
- The user runs `skaffold dev`. Make changes for that workflow; do not start
  another dev loop or deploy the application unless requested.
- Preserve unrelated work in this working tree. Do not prune Docker objects,
  clear caches, delete files or reset changes as routine troubleshooting.
- Never use `/dev/shm` as build scratch space. `/tmp` is a small tmpfs here.
  When permitted, use the existing `/home/lauri/.cache/slopify-build-tmp` as
  `GOTMPDIR`. Request the normal sandbox approval for writes outside the repo.
- Keep verification proportional. The user prioritizes trying playback over
  long test runs. Compile changed services, syntax-check JavaScript and render
  or lint changed Helm templates; use targeted tests when needed to establish
  correctness. Historical test fixtures may predate the current auth design.
- Do not run Skaffold with debug verbosity: it can print secret environment
  values. Avoid dumping environment variables or Kubernetes Secret contents.
- Investigate restart reasons before blaming reconnect logic. An SSE retry
  cannot recover an unavailable backend. Do not claim live recovery from a
  successful local build; verify rollout separately if authorized.

## Architecture and ownership

Services communicate over HTTP/JSON and Server-Sent Events, without a broker.
All device connections are outbound HTTPS through the public ingress. No
SlimProto, LAN discovery, extra public ports or per-device LoadBalancers.

| Component | Source | Responsibility / storage access |
|---|---|---|
| indexer | `cmd/indexer` | Only writer to the media tree; UID/GID 1000. Probes files, watches changes, writes sidecars, publishes library events. |
| catalog | `cmd/catalog` | Read-only media; builds an in-memory searchable catalog from sidecars. |
| streamer | `cmd/streamer` | Read-only media; serves signed originals using HTTP range requests. |
| players | `player/s` | Per-user queues, output selection, pairing and playback state; persists `/data/players.json`. No media mount. |
| frontend | `cmd/frontend`, `web/` | Static UI and OIDC login/token renewal. No API proxy or BFF. |
| transcoder | `cmd/transcoder` | Per-profile HTTP workers fetching signed originals. No media mount. |
| device player | `player/` | Plays originals and LAN senders directly to ALSA, runs under systemd. Decodes FLAC, WAV and MP3 itself; other codecs use mpv. |

The gateway was removed. Do not restore forwarded-user-header authentication or
route all APIs through frontend. An enricher and S3 backend are future work,
not implemented components.

The device player is for music: audio-video sync and gaming are not priorities, so don't
trade quality or reliability for lower latency. Device-player priorities are (1) lossless, native-rate playback of Slopify
originals, including full decoded precision, and (2) laptop audio over the LAN.
Lossless 16/24-to-32 packing is allowed; silently resampling originals or reducing
S32 to float32 precision is not. Sender desktop mixes are a different guarantee:
report their actual received format and further conversion. Receiver breadth is
secondary. See the project goals in the local
network audio design.

`player/localui.go`, `localsources.go` and embedded `player/ui/` serve an
opt-in trusted-LAN speaker UI (`-local-listen`), with status/SSE, sender setup,
hardware volume/native playback-switch mute, and opt-in source
selection (`-local-source-control`).
There is no login; mutations require same-origin JSON and a custom header.
Keep startup/buffering status inside the Slopify source card and omit its local
hostname. PCM labels use a shared renderer: ALSA spellings normalize to abbreviations
such as S32LE, with precision, signedness and byte order in tooltips. Hardware controls belong in the amplifier box; use a standalone Output
section only when source control is unavailable. Source cards expose descriptive metadata as key: value lines: queue entry fields
for Slopify, bounded allowlisted media properties for LAN streams. The UI hides
technical queue fields from this list because format is summarized separately. Never include
file paths, stream URLs or arbitrary application properties; render text only.
Source settings use expected revisions. Local UI mutations have an eight-second
deadline and are never replayed automatically; a timeout does not prove the
command was rejected. Speaker tabs share one background SSE connection via SharedWorker where
available, with a direct EventSource fallback; visibility does not disconnect
status. Start the connection after document load and release the tab port on
pagehide; the worker closes SSE only after the final tab leaves.
Pinned sources disable automatic selection and its silence/sound guard. Hardware
volume and native mute live in the amplifier box when supported. The Sources diagram also holds output
format; clipboard fallback supports LAN HTTP. Off pins the selected source; Recent
uses a −60 dBFS threshold, a persisted 0–2000 ms sustained-sound guard (default
500 ms, Recent only), and two-second silence hold; Priority follows the
persisted card order and holds the selected source through silence/node gaps
for prioritySilenceMs (0–120000, default 30000), independently of the Recent guard.
Expose a fallback target/deadline only during this hold with a fresh sounding
lower-priority candidate; render its countdown using server time from SSE. `switchNow` validates the
current revision and fresh pending fallback, expires the old silence hold,
and selects that fallback without pinning. Explicit paused queue state is
tracked independently from decoder buffering/idle and bypasses automatic
silence/startup holds; a manual pin still wins.  Missing meter data is unknown, not silence; preserve
recency across meter recreation. Click pins, drag reorders, and keyboard
alternatives remain.
The bounded registry reserves Slopify plus seven LAN slots, admits the newest session per peer, retains disconnected
peers, and stores policy in `-local-state` (empty means memory only). Never use
sender names as authentication or unbounded metric labels. Show source IP/title
only in local status, never tokens or signed URLs. Keep receiver inventory and
arbitration out of cloud APIs; external-source availability reporting is deferred.

A pinned source retains a dashed connection line while disconnected or buffering,
becoming solid when its amplifier link is active. The sender setup uses Zeroconf
native and Fixed tabs without an outer panel. AirPlay appears only with
`-local-airplay`, indicating a configured receiver (not a health check); nothing
provides one until the AirPlay bridge exists. mpv cache stalls report
buffering in the source card as well as initial track loading.

Automatic selection keeps the current source playing while Slopify buffers
privately; startup alone never takes priority. Selection waits for fresh sound.
A selected stream has a bounded three-second first-meter grace after playback
readiness, and Priority's silence allowance starts at readiness or the last
audible observation, whichever is later. Explicit pins still select immediately.

Source switching closes the device, withdrawing whatever it still held, before the
next source opens it at that source's own format.

The local page puts automatic selection below the title divider, without an
enclosing Sources panel. Source and amplifier cards use equal-width desktop
columns and stack on narrow screens. Source charts are compact; the output
chart stays taller. Pending spinners sit at the top right, with explanations
replacing the stream state. Status lines use `2ch · 96kHz · S32LE · running`;
tunnel IPs occupy their own line. The output box is titled with the ALSA PCM name, with its /dev node below. Source cards label their protocol like the output's eyebrow.
The Slopify card title beside its padlock is the configured server hostname,
without URL credentials, path or query; do not repeat it in the details.
Sender instructions include per-user and system-wide persistent Pulse zeroconf
discovery with copy buttons. System-wide configuration applies to each user
session; it does not create a system audio daemon.

The local page heading and browser title use SLOPIFY_NAME (hostname fallback).
Hide the connected-status message but retain failure feedback. Use HiFiBerry
capitalization in display names while preserving lowercase system IDs.

`playback.go` separates queue orchestration from backend/session lifecycle. The
default `native` engine (`native.go`) decodes with `player/internal/audiodec`: integer-only
FLAC, WAV and Q28 MP3 (`mp3tables_gen.go` generates its tables). Decoding must not use
floating point; compare against ffmpeg in its tests. MP3 has no bit-exact definition,
so its accuracy is a measured tolerance; intensity stereo and mixed blocks have no
vectors yet. Output is `player/internal/alsa` (pure-Go ioctls, hw: devices, no conversion,
exclusive use) or null. Mixer controls use the same package, never amixer. Verify with
`SLOPIFY_TEST_ALSA_LOOPBACK=Loopback` on an idle snd-aloop card, and
`SLOPIFY_TEST_ALSA_CONTROL=card:Name`, which only rewrites current values.

The player has no PipeWire client, and the speaker runs no PipeWire. Don't restore it.
AirPlay is unavailable: the planned bridge is shairport-sync's Pulse backend into
`player/internal/pulse`, with a pure-Go receiver as the long-term option. `player/internal/pulse` is a
playback-only Pulse native-protocol server with no resampling, mixing, DSP or floating-point maths; float32 from clients such as Chrome is unpacked with integer operations.
`internal/esound` is an opt-in playback-only ESound server (`-esound-listen`), also
unauthenticated: 8/16-bit mono/stereo streams only, paced by TCP backpressure, with no
samples, recording or monitoring. `internal/upnp` plus `player/upnp_source.go` is an opt-in UPnP AV renderer (`-upnp-listen`)
for controllers such as swyh-rs: SSDP replies, AVTransport, RenderingControl and
ConnectionManager, without eventing or NOTIFY. It pulls FLAC, WAV/RF64 or raw L16/L24 over
plain HTTP; dial-time checks keep streams on private addresses, redirects are refused and
only the host is logged. `liveStream` lets all these protocols share source cards.
`ir_amp.go` and `internal/lirc` drive a motorised amp knob over one-way IR (`-ir-device`), with
NEC encoding left to the kernel through LIRC scancode mode. Position is only known by counting
from a completed sweep to zero; any send failure makes it unknown, and it must never count up
from an unknown position.
Keep the Pulse start threshold reachable (below target minus minReq): otherwise a stream
stalls after an underflow. PipeWire tunnels answer an underflow by rewinding and
resending, heard as looping, so never read a live stream faster than real time
(`nativeFillPacer`). Never report device delay in latency replies, and never buffer beyond what a tunnel asks: it steers reported latency plus its ring to 200 ms, and past that it drains the ring and sends silence, heard as steady ticks. For A/V sync, shrink the delay instead with `-live-device-buffer-ms`.
`nativeRouter` owns the device: only the selected source writes, others keep real-time
pace and stay metered. Sender keys are stable across addresses, so slots carry over.
Hardware level changes move the slider of only the sender connected to the amp; a
sender already at that level is left alone so that a drag never fights its echo.
Each card shows its own "Buffer" at its bottom. Speaker `/metrics` (`native_metrics.go`)
separates network, stream queue, device queue and scheduling; labels are bounded source
slots, never sender names. Peak windows are stamped on the audio timeline and released
once heard; the chart fills adjoining windows and leaves real gaps visible.

mpv is the fallback for other codecs and a selectable engine: one headless subprocess
across sessions with private inherited JSON IPC (no listener, URLs never in argv/logs).
Commands and events are bounded; IPC failure discards the connection rather than
replaying commands. Only one upcoming signed original is handed to it, within 30 seconds
of the transition when duration is known. Force S32, unity volume and native rate;
validate reported format at playback restart, not during transient reconfiguration.
Use Ubuntu's mpv package. It opens ALSA itself, outside `nativeRouter`, so it can
conflict with a LAN source over the device.

`slopify-pulse-advertise.service` publishes the Pulse endpoint with
avahi-publish-service, using SLOPIFY_NAME as service instance and description and
slopify.hifiberry as the actual remote target. Keep using Avahi; it provides the exact
friendly instance name. Advertise IPv4 to match the IPv4 Pulse binding. `player/internal/receiverconfig` shares `SLOPIFY_PULSE_ENABLED` and `SLOPIFY_AIRPLAY_ENABLED`
deployment flags for the player setup UI. `player/airplay.go` accepts bounded loopback-only UDP
Shairport metadata, allowlisting sender ID/name/software, IP and track tags;
never expose Active-Remote credentials. A one-slot, 100 ms volume worker maps
AirPlay -30..0 to the configured ALSA range; -144 uses native mute (minimum
level fallback without a playback switch). Keep Shairport software gain unity.
Sender metadata decorates the existing card; splitting cards
and pushing knob changes back to AirPlay senders are not implemented.
Discovery is not authentication. Keep manual IP
tunnel instructions for routed networks; do not configure multicast reflection.

Source selection and queue state remain distinct. The local source-pause worker
coalesces selection intents independently of routing. Request a conditional
pause only when selection transitions away from Slopify, never on each later
Play, track change, or restored startup selection. Failed mutations are not
replayed against newer playback decisions. Its cloud pause command is `pause-if-sole-output`; the server checks selected membership
and playback revision under its queue lock. Multiple selected outputs keep
playing (including this device's paced, unheard stream). Selecting Slopify never
implicitly resumes a paused queue. No receiver inventory goes to the server.

Everything specific to the device player lives in `player/`: the main package, its
`internal/` packages, `deploy/`, `openwrt/` and `shairport/`. Server images never copy it.
Only `internal/httpx` and `internal/volume` are shared with the servers.

`player/openwrt/` builds Raspberry Pi 3 and Cubietruck images with the official Image
Builder, adding a static player compiled outside it through `FILES=`. It runs as nobody with group audio. Keep it free of
SDK builds and custom packages; images have no mpv, so FLAC, WAV and MP3 only. The LuCI
panel is plain files under `files/`; `-list-audio` gives it cards and volume controls. Inputs are
kernel names resolved by the player, with no input hotplug: init opens only configured devices.
`.github/workflows/openwrt.yml` builds both as artifacts.

`player/deploy/` is the supported Ubuntu 26.04 Pi install: `overlay/` mirrors the root
filesystem and `install.sh` installs it with the player. Keep this directory free of
private addresses and device names. The prohibition on committing `deploy/raspi` remains.

Proposed device-local AirPlay, Ubuntu/Pulse-compatible, Android and Windows
sources are described in `docs/design/local-network-audio.md`, including auth,
source arbitration/pinning, precision release
gates, Ubuntu setup, rotary actions, local UI and metrics. Its PipeWire engine was
built and then removed, so treat those parts as history. This is a
design proposal, not current functionality or authorization to expose new LAN
ports. Keep its local receiver boundary separate from cloud service ingress;
`deploy/raspi` remains an uncommitted experiment for later custom-image work.

Ingress routes `/api/catalog` to catalog; `/api/players`, `/api/stream` and
`/device` to players; `/stream/original` to streamer; `/stream/opus-128` and
`/stream/mp3-192` to separate profile pods. `/stream/video-aac-192` and
`/stream/video-copy` share the video worker. `/` and `/auth/*` reach frontend.
Profile names include bitrate where applicable and are bound by URL signatures.

## Library and metadata invariants

- Keep filesystem access behind `os.Root` and `internal/sidecar`. Validate
  library-relative public paths with `media.CleanPath`.
- Metadata is per-directory `.slopify/dir.json`, with no persistent central
  library database. Catalog's search index is disposable in-memory state.
- Only indexer writes sidecars. Never delete media. When the last item goes
  away, leave an empty sidecar and its metadata directory. Permission failures
  must not trigger cleanup or terminate the whole indexer.
- Sidecar writes use `dir.json.tmp` followed by atomic rename. Transcode cache
  writes use `.part` followed by rename; these are separate mechanisms.
- Ignore files/directories beginning with `.` in normal library enumeration,
  watcher events and public media paths. Access sidecars explicitly.
- Preserve valid UTF-8 tag text. Run ffprobe JSON with string_validation=ignore
  and decode otherwise-invalid bytes as ISO-8859-1 before Go JSON decoding.
  Do not rewrite source audio to fix tags. Reject replacement-character tags
  for display, falling back per field to filename heuristics. TagEncodingVersion
  allows one targeted retry of previously corrupted tags, without a full
  100,000-file probe-version bump. Repair stale queue labels on load/add too.
- Tags are optional; path heuristics fill missing fields. Video titles use
  container tags or filename guesses, never audio-track labels such as DTS.
- Sidecars store bitrate, sample rate, channel count, decoded bit depth, first
  audio/video codecs and all audio/video stream codec names. Queue metadata
  avoids remote ffprobe at device playback startup where available.
- Probe cache validity includes size, mtime and a media-kind probe version.
  Persist failed probes too, so unchanged failures are not executed repeatedly.
  `INDEXER_REPROBE=true` explicitly retries them. A schema version bump causes
  a one-time re-probe: consider the cost on a large library.
- Indexer uses two ffprobe workers by default (`INDEXER_PROBE_WORKERS`, 1–16).
  Probing opens a file through `os.Root` and passes `/dev/fd/3` to ffprobe.
  Keep errors useful: exit code, bounded stderr and affected paths in logs.
- Report failures once per directory per indexing pass, with at most three
  error samples. Keep HTTP health/readiness/SSE independent of background
  scanning, including scans with failures.
- Catalog batches changes into at most four index rebuilds per second. It
  reloads sidecars on subscription reconnect because events may have been lost.
  Reconnect backoff caps at five seconds. SSE hub heartbeats every 15 seconds;
  subscriber header timeout is five seconds and read-idle timeout 45 seconds.

## Authentication and pairing

- OIDC only, with Passmower `OIDCClient`; no local accounts or passwords.
  Frontend owns authorization-code/PKCE login and renewal. Refresh tokens stay
  in an encrypted HttpOnly session cookie; access tokens live in browser memory.
- Catalog and players independently verify bearer access tokens: issuer,
  signature, expiry, not-before, API audience, access-token type and scopes.
  Ignore client-supplied identity headers. API audience is the public origin
  plus `/api`. Only frontend receives the OIDC client secret and session key.
- Browser SSE uses fetch with an Authorization header, not tokens in query
  strings. Expired authentication ends subscriptions so they can renew.
- Devices generate their own pairing token, normally persisted at
  `/var/lib/slopify-player/token`. Server stores its SHA256 hash in player state
  and strips it from public responses. Devices need adoption in the web app.
- Hardware pairing atomically binds a pending device to the authenticated user's
  queue. Claimed devices are visible/selectable/manageable only by that owner.
  Unclaimed devices remain discoverable to authenticated users (first claim wins).
  ReportedName is device-local identity; default web Name is owner display name
  plus ReportedName, while NameSet preserves a separate full web override.
  Persist verified OwnerIssuer/OwnerSubject, strip both from public JSON. Migrate
  legacy devices only from a unique selecting user queue; otherwise require pairing.
  Current queue keys remain subject-only under one configured issuer. Do not add
  multiple issuers or change issuer without an explicit account-state migration.
  Federation and separate-IdP grants are proposed in `docs/design/federation.md`;
  matching emails/names never link identities or confer speaker/library access.
- Signed media URLs grant temporary access without OIDC cookies. Do not log
  their signatures, embed bearer tokens in exports or trust request Host as
  the canonical origin. PUBLIC_ORIGIN comes from the OIDC_CLIENT_ORIGIN secret
  for absolute playlist URLs; API audiences expand that origin plus /api.
- M3U and PLS exports snapshot the entire present queue and always use signed
  originals, valid for 12 hours. They do not synchronize mpv/VLC with Slopify.

## Queue, browser and device playback

- One durable logical user queue per OIDC subject; browser tabs and Linux
  devices are replaceable outputs. The queue follows the account across tabs
  and phones. Keep ownership checks on commands and browser reports.
- Preserve the current entry separately from the remaining queue. Clearing
  the queue or removing its playing item must not interrupt playback.
- Users can select multiple outputs. One primary output advances the queue.
  This is shared control, not clock- or sample-synchronized multiroom audio.
- Opening another tab must not move playback. An explicit Play action may
  add this browser when every selected output is an offline browser, retaining
  their selections so they can rejoin; never
  steal from a connected output or silently replace a hardware device.
- GET/HEAD requests retry transient proxy errors (502/503/504) and network
  failures across redeploys. Auth-token renewal is explicitly retryable; queue
  mutations and link creation are never replayed automatically. Cancel obsolete
  library reads and refresh the library after SSE reconnect. Cached app assets
  cover transient frontend errors; codec fallback must distinguish proxy outages.
- Browser periodic progress reports run only while the document is visible;
  foregrounding sends a fresh position. Playback events (including ended and
  lock-screen controls) still report while hidden. This is not a guarantee
  against mobile OS suspension or SSE disconnects.
- Browser disconnects have a 30-second grace period before pausing the output.
  Retry transient network errors from the last played position, including
  Chrome's ERR_NETWORK_CHANGED; do not treat network failure as user pause.
- On a current-entry change, centre the playing queue row in its own scroll
  viewport where possible. Never steal keyboard focus or scroll on position-only
  updates; respect reduced-motion preferences.
- Repeat is a queue setting copied to outputs: `after`/`before` wrap, for automatic
  advance and Next/Previous. Devices only prefetch up to the queue tail, so wrapping
  restarts through the server rather than gaplessly.
- Library + always appends to the queue tail. The append command ignores
  insertion hints; use insert for drag/drop placement and next-up for Play next.
- Library rows have explicit add/play actions. Search toolbar defaults to the
  results; selecting a row targets that row. Cap additions at 300 tracks and
  deduplicate search results by path. Folder navigation uses breadcrumbs and
  recursive folder additions, not a nested directory tree. Omit the redundant
  “Folder” metadata label beside folder add buttons.
- Link the web app’s Slopify header title to GitHub; keep the GitHub icon and
  `codemowers/slopify` repository link in the local speaker UI footer.
- Device/output selection, browser audio quality, appearance and nerd mode
  live in settings. Nerd mode is a third desktop column, stacked on narrow
  screens. Show source audio/video codecs in file tooltips.
  Dock video below the scrolling library on desktop, above playback controls
  on mobile. Preview has no duplicate title/transport/header buttons; clicking
  it opens native fullscreen controls. Output selection is top-right in the
  page header. Selected offline outputs stay visible, grayed out, and sync to
  current queue state on return. Queue rows omit position numbers and use selection and per-row
  actions without checkboxes; coarse-pointer taps toggle selection.
- Browser video tries originals when its audio codec is supported; rejected
  containers can fall back to MP4 with both codecs copied (`video-copy`).
  Unsupported audio uses stereo AAC 192 kbps (`video-aac-192`). Never transcode
  video. Source video codec support remains a browser/hardware limitation.
  HEVC in MP4 is tagged `hvc1` when source metadata identifies HEVC.
- Transcoded/remuxed video uses fragmented MP4. Seeking opens a fresh stream
  with audio preroll aligned to a source video keyframe; it is approximate.
  Original streams use ordinary HTTP range seeking. Keep errors visible.
- Devices always fetch originals, including video containers, and play audio only
  (mpv `--vid=no --aid=1`). No video rendering in the Go device player.
- Preserve device sample rate and decoded precision. No resampling, software
  volume or lossy conversion. Lossless wider-word packing and mono duplication
  are allowed to match hardware. Unsupported sample rates are errors.
  Use ALSA `hw:` outputs to avoid implicit ALSA conversion.
- Prefetch the next track for gapless device playback; reopen the DAC only on
  format changes and release it while paused. Preserve detached current entries
  in device queue views when the visible queue is cleared.
- Device hardware volume is opt-in with `-volume-control` and `-volume-card`;
  without flags, never read or set mixers. `-volume-min`, `-volume-max` and
  `-volume-initial` use alsamixer's perceptual displayed 0–100 scale (defaults
  0, 40, 20), not raw hardware-register levels. Map levels to hardware using
  ALSA dB metadata; 20/40 on Amp2 correspond to roughly -42/-24 dB.
  Set the initial level once per process start; poll local mixer changes each
  second and enforce bounds. Advertise capabilities/dB steps on device SSE
  connection and report observed levels, hardware dB and mute through `/device/v1/volume`. The players
  service publishes them over SSE. Web volume readouts and accessible slider
  text show only dB (−∞ dB for mute), never ALSA level numbers/control names.
  Volume commands require the output's current account selection and use independent revisions; never alter playback versions
  or queue state. Linux `-rotary-input`/`-button-input`/`-previous-input`/`-next-input`/`-mute-input` consume explicitly
  configured evdev devices (REL_X and KEY_PLAYPAUSE), never scan keyboards.
  Bare Amp2 encoder wiring: BCM23/24 rotation, BCM25 push, commons to GND;
  rotary-encoder/gpio-key boot overlays provide pull-ups. Stable udev symlinks
  grant read access only to these inputs. Device `/device/v1/control` permits
  only version/entry-checked play/pause, next and previous of the queue selecting that device.
- Local rotary actuation has a dedicated goroutine, separate from playback
  commands/reports. Never wait for HTTP between detents. Volume reporting uses
  a separate goroutine, one-slot notifications, current snapshots and 100 ms
  coalescing; failed reports retry the latest state rather than historic levels.
  Button delivery must not block rotation when playback/network control stalls.
- Output selection uses icon buttons with `aria-pressed` and immediate feedback.
  Serialize/coalesce rapid selection changes without replaying failed mutations;
  update keyed device cards in place so playback SSE does not replace sliders.

## Shared browser output links

- Creating/rotating a link requires both players:write and stream scopes.
  Authenticated owners create named shared browser outputs with a 256-bit
  capability; only its SHA256 hash persists in player state. Public player
  JSON must strip both device token hashes and link hashes.
- Link format is /output/#<output-id>.<secret>. Keep secrets in URL fragments
  and bearer headers, never API query strings or logs. PUBLIC_ORIGIN is not
  needed to create relative links. Public page is served without OIDC.
- /output-api/{id}/{events,stream,report} accepts only that link's capability.
  No commands, library enumeration or arbitrary-path signing. Stream signing
  accepts only the output's current entry and issues five-minute media URLs.
- Public UI is read-only. Local autoplay permission and fullscreen are allowed;
  no queue/play/pause/seek controls. The owning OIDC account retains its queue.
- One link supports up to 16 concurrent receivers grouped as one named output.
  Sinks are always available/selectable, including with zero sessions; show
  session counts instead of online/offline. Connected remains an internal
  physical-session signal. Web sinks have their own kind; API availability is
  a separate derived available field. Do not
  auto-transfer a zero-session sink to the controlling browser.
  Each tab has a random receiver ID; only one receiver's telemetry drives
  position/track completion, with failover on disconnect. This is not sync.
  With no active receivers, server-side known durations advance the queue;
  disconnecting the final receiver never pauses a persistent sink.
- Owner can rotate the token (New link) or delete the output (Revoke). Close all
  receiver streams on rotation/deletion. Already signed media URLs expire
  independently. Empty shared outputs must survive persistence/restart.
- Link reports cannot command state changes, seek arbitrarily or skip early.
  Validate report versions/entries/position before accepting telemetry.
- Keep shared outputs scoped to their owner in the authenticated output list.
  The main app has a creation form and revocation controls in device settings.

## Transcoding and storage

- Keep ffmpeg/ffprobe subprocesses; no C/C++ or CGO rewrite yet.
- Opus 128 and MP3 192 run in separate pods, each fetching originals over
  signed HTTP and owning a 25 GiB PVC. Cache limits are 25,000,000,000 bytes
  each, including partial writes; total application budget is 50 GB.
- Cache storage uses the `transcodes` TopoLVM/XFS StorageClass with Delete
  reclamation and WaitForFirstConsumer binding. Its manifest is cluster-owned
  in `deploy/storageclass-transcodes.yaml`, outside the app chart.
- Completed cache entries are keyed by source ETag (size/mtime), profile and
  encoder args. Protect open readers from eviction. Never publish interrupted
  or failed encodes. Cache errors must not interrupt a live response.
- Each pod defaults to two encodes. Identical concurrent cache misses share one
  job; followers currently wait for completion. Hits bypass encoding and support
  ranges. Seek-offset streams bypass caching.
- Video worker serves `video-copy` and `video-aac-192`, shares its concurrency
  slots between them and has no disk cache or PVC. Do not consume the audio
  cache budget with copied movies.
- Workers can fetch originals from other nodes; node-local cache PVCs still
  constrain scheduling. No distributed-job scheduler is implemented.

## Observability and resource context

- With network policy enabled, all Slopify pods default-deny egress. Indexer
  and streamer have none, including DNS. Catalog reaches indexer events;
  transcoders reach originals. Catalog/players/frontend have DNS and explicit
  networkPolicy.oidcEgress rules. Never allow general internet HTTPS. Replies
  to permitted SSE/metrics/HTTP ingress remain allowed. Keep ffprobe local-only.
- Indexer metrics use a separate internal listener, `METRICS_LISTEN_ADDR=:9090`;
  app health/events use `:8080`. No public metrics ingress or metrics Service.
- Production enables a PodMonitor. Skaffold Helm rendering explicitly includes
  `monitoring.coreos.com/v1/PodMonitor`; network policy permits monitoring pods.
- `slopify_indexer_files` and `slopify_indexer_directories` are current gauges
  reconstructed while loading/writing sidecars, including empty directories.
  `*_files_indexed_total` and `*_directories_indexed_total` are process-lifetime
  indexing-pass counters, including successful unchanged sidecars and probe
  fallbacks. They are not unique-file totals across restarts.
- Both file metrics have `mimetype`, `audio`, `video` labels. MIME is classified
  from the filename; stream labels are a codec name, empty for no stream,
  `multiple` for two or more streams (even the same codec), or `unknown` for
  unavailable/stale probe information. Use `sum(slopify_indexer_files)` for the
  current total. Never use filenames or paths as metric labels.
- Export errors by operation/reason, probe failures, scan duration/in-progress,
  last successful full-scan timestamp and watched-directory count. A scan with
  errors does not advance last success. Directory errors do not stop serving.
- Indexer has a 512 MiB container limit, 128 MiB request and `GOMEMLIMIT=128MiB`
  to leave room for ffprobe children. It previously OOM-looped at 256 MiB.
  Go's soft memory limit does not bound child processes or guarantee no OOM.

## Deployment and configuration map

- Repository is `codemowers/slopify`; active chart lives in `chart/`. Read
  the gitignored `AGENTS.local.md`, if present, for private cluster, namespace,
  registry and host details. Never commit cluster names, namespaces or hosts of
  private installations. Do not infer current rollout from these files.
- `skaffold.env` (registry, context, namespace, cleanup disabled),
  `chart/values-local.yaml` and `chart/values-demo.yaml` are gitignored; tracked
  `*.example` files document them. `domain` is the public hostname for
  ingress and the Passmower OIDCClient; any cluster-specific hostname
  expansion belongs in private values. Public origin is read from the generated
  OIDC_CLIENT_ORIGIN secret, never a second domain value.
- allowedUsers is supplied through private installation values, never hardcoded
  in OIDCClient templates. Empty lists fail rendering rather than opening access.
- Chart requires Passmower OIDCClient and Mittwald StringSecret controllers.
  Generated signing/session keys and queue state are persistent. Media may
  use hostPath or a PVC; only indexer mounts it read-write.
- Per-service Dockerfiles in `build/` cover cluster services only, with explicit
  input copies to avoid unrelated rebuilds. `.dockerignore` allows only build
  inputs. Services without ffmpeg use static Go binaries in scratch.
  Indexer/transcoder use Alpine plus ffmpeg. Do not restore a generic root
  Dockerfile. `build/player.Dockerfile` builds the device player, which normally runs under systemd or procd; it has no mpv.
- `go.mod` targets Go 1.27.1.
- Experimental, uncommitted Pi service/configuration files live in `deploy/raspi/`: a generic
  systemd unit, explicit Amp2 volume/rotary drop-in, udev input permissions,
  headless boot snippet/configurator and static Ethernet example. Installer
  restarts only the player; boot and network changes are separate, never an
  automatic reboot. Keep real server/IP settings in gitignored
  `deploy/raspi/local/`; never version pairing tokens or compiled player binaries.
  Do not commit this experiment; the supported install is `player/deploy`.
- Demo: `deploy/build.sh` uses Kaniko in `$KUBE_CONTEXT`/`$NAMESPACE` and
  `chart/values-demo.yaml`. It uploads context as a ConfigMap, avoiding
  `kubectl exec`. Partial builds change only selected deployment images.
  `deploy/seed.yaml` seeds sample media.
  Browser playback mechanics have focused Node tests in web/media.test.mjs;
  do not recreate legacy tests that forge gateway cookies or mount session keys.

| Environment | Components | Default / purpose |
|---|---|---|
| `LISTEN_ADDR` | HTTP services | `:8080` |
| `MEDIA_ROOT` | indexer, catalog, streamer | `/media` |
| `INDEXER_EVENTS_URL` | catalog | unset disables updates; chart sets indexer `/v1/events` |
| `STREAM_SIGNING_KEY` | players, streamer, transcoder | required shared signing secret |
| `OIDC_ISSUER`, `OIDC_AUDIENCE` | catalog, players | required; origin plus `/api` audience |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URI`, `SESSION_KEY` | frontend | required OIDC/session config |
| `PUBLIC_ORIGIN` | players | required for absolute playlist exports |
| `ORIGINALS_URL` | transcoder | `http://slopify-streamer` |
| `TRANSCODE_PROFILE` | transcoder | profile for this pod; video worker also serves copy profile |
| `TRANSCODE_CACHE_ROOT`, `TRANSCODE_CACHE_MAX_BYTES` | transcoder | `/cache`, `25000000000`; zero disables cache |
| `TRANSCODE_CONCURRENCY` | transcoder | `2` |
| `INDEXER_PROBE_WORKERS`, `INDEXER_REPROBE` | indexer | `2`, `false` |
| `STATE_FILE` | players | `/data/players.json` |
| `SLOPIFY_SERVER`, `SLOPIFY_NAME`, `SLOPIFY_OUTPUT` | device | URL (empty with `-local-listen`: LAN sources only), hostname, `aplay:hw:0,0` |
| `SLOPIFY_MPV` | device | `mpv` |
| `SLOPIFY_TOKEN_FILE` or `SLOPIFY_TOKEN` | device | persistent token file or explicit token |

Use `SLOPIFY_OUTPUT=null` for a device without audio hardware. Server health
checks must not wait for an initial library scan. Keep deployment specifics
current in values files rather than adding hard-coded locations to services.
