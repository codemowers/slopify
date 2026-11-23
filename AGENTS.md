# Agent guide

Keep this file for implementation decisions, repository navigation and working
context. README.md is for people evaluating, deploying and using Slopify.
Update both when behavior changes; do not append task transcripts to either.
User instructions take precedence over these defaults.

## Product priorities

- First-class browser usage is the primary product: desktop/mobile library,
  search, queues and playback. The dedicated local audio player is second,
  filling a squeezelite-like role with our own HTTP protocol and first-class
  Raspberry Pi 3/4/5 support. Keep the local player UI minimal: volume, mute and
  play/pause; previous/next are planned and selecting an existing queue is only
  a possibility. Never add catalog browsing, adding tracks or queue editing to
  the local player UI; these belong in the desktop/mobile browser UI. Existing
  physical-input/protocol transport capabilities do not expand this UI scope.
  Bluetooth audio is out of scope; do not keep it on the roadmap.
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

- `issues/` (gitignored) holds drafts of upstream bug reports, not yet filed, to investigate later.
  Each says where the behaviour is reproduced here. Filing one is outward-facing: ask first.

- Use Go for services and tools. Browser code is plain JavaScript/CSS/HTML in
  `server/web/`, embedded by `server/cmd/frontend`. Do not introduce a frontend framework,
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

## Repository layout

- `server/`: service commands, server-only internal packages, embedded browser
  UI, Dockerfiles and deployment helpers. Keep Skaffold at the root.
- `player/`: Go speaker and its internal packages, Linux service installation
  and monitoring tools. Build it with `go build ./player` from the root.
- `desktop/`: GNOME extension and per-user installer.
- `appliance/`: OpenWrt Image Builder, LuCI files, profiles and experimental
  source builds. These build the player; Go playback stays in `player/`.
- Root `chart/` contains the deployable Helm chart.
- Root `internal/httpx` and `internal/volume` are shared. Keep one root Go module.
- READMEs explain the product, goals, features and getting started. Detailed
  operation/design belongs in each component’s `docs/`; implementation rules
  belong in AGENTS.md.
- HiFiBerry examples live in `appliance/docs/hifiberry-amp2/` (rotary) and
  `appliance/docs/hifiberry-amp4/` (GPIO25 IR receive). Do not mix their GPIO25 assignments.
- Private values stay under root `chart/`; never stage
  local/demo installation values or `values-demo.example.yaml`.

## Architecture and ownership

Services communicate over HTTP/JSON and Server-Sent Events, without a broker.
All device-to-server connections are outbound HTTPS through the public ingress. No
SlimProto, server-side LAN discovery, extra public ports or per-device LoadBalancers.

| Component | Source | Responsibility / storage access |
|---|---|---|
| indexer | `server/cmd/indexer` | Only writer to the media tree; UID/GID 1000. Probes files, watches changes, writes sidecars, publishes library events. |
| catalog | `server/cmd/catalog` | Read-only media; builds an in-memory searchable catalog from sidecars. |
| streamer | `server/cmd/streamer` | Read-only media; serves signed originals using HTTP range requests. |
| players | `server/cmd/players` | Per-user queues, output selection, pairing and playback state; persists `/data/players.json`. No media mount. |
| frontend | `server/cmd/frontend`, `server/web/` | Static UI and OIDC login/token renewal. No API proxy or BFF. |
| transcoder | `server/cmd/transcoder` | Per-profile HTTP workers fetching signed originals. No media mount. |
| device player | `player/` | Plays originals and LAN senders directly to ALSA, under systemd or procd. Decodes FLAC, WAV, MP3 and Ogg Vorbis itself; unsupported codecs fail visibly. |

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
such as S32LE, with precision, signedness and byte order in tooltips. Lossy input
labels expose `decodedBits` as “24-bit decoded”, never as the recording’s bit depth. Hardware controls belong in the amplifier box; use a standalone Output
section only when source control is unavailable. Source cards expose descriptive metadata as key: value lines: queue entry fields
for Slopify, bounded allowlisted media properties for LAN streams. The UI hides
technical queue fields from this list because format is summarized separately. Never include
file paths, stream URLs or arbitrary application properties; render text only.
Source settings use expected revisions. Local UI mutations have an eight-second
deadline and are never replayed automatically; a timeout does not prove the
command was rejected. Each speaker tab uses a direct EventSource, started after document load and
closed on pagehide. Keep the newest three streams; evict the oldest on a fourth
connection. Evicted tabs close EventSource without retrying, preventing reconnect
churn. Visibility does not disconnect status.
Output history is full on SSE connection, then incremental (`outputDelta`);
source charts likewise batch all 200ms windows (`windows`/`windowsDelta`) instead
of sampling the latest level at the SSE rate. Preserve real gaps; never bridge
missing observations just to conceal slower status delivery.
Chart windows expose integer `db` (-127..0 dBFS, -128 for silence); keep
absolute S32 PCM magnitudes (`uint32`, including MinInt32) private for arbitration.
Meter callbacks and the -60 dBFS comparison (2147484) stay integer; derive the
display value with generated integer thresholds, never runtime logarithms.
Quantization applies only to display.
Source `input` counters are encoded bytes and decoded audio frames with their
sample rate. The browser derives average compressed input bitrate, independent
of wall-clock playback and download bursts. MP3 accounts complete frames,
excluding metadata/prefetch; other native originals use decoder-reader bytes.
Keep this available with Prometheus disabled and distinct from decoded PCM rate.
The decoded PCM queue has 32 blocks of 16384 S32 samples (2 MiB), independent of
the download budget. Fill blocks across short decoder reads before enqueueing;
otherwise MP3's 1152-frame reads exhaust the slot count at about 800ms despite
the advertised 5.9s stereo/44.1kHz capacity. Flush partial blocks at EOF/error.
SSE starts with `{snapshot: ...}`, followed by `{patch: [[JSON-pointer, value], ...]}`;
a one-element patch tuple deletes the property. Only changed values are serialized.
Keep the latched wire state separate from reconstructed chart history in
each direct connection (`status-state.js`); `player/test-status-state.mjs`
checks browser latching, removals and reconnect resets.
if a cursor falls outside the retained history, send a fresh full snapshot.
Each new connection receives the full retained history.
`-local-sse-interval` (200ms default, 100ms–10s) controls status serialization only;
OpenWrt UCI uses `local_sse_interval_ms`. Keep 200ms audio peak windows independent.
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
The bounded registry reserves Slopify plus 30 LAN slots, admits the newest session per peer, retains disconnected
peers, and stores policy in `-local-state` (empty means memory only). Never use
sender names as authentication or unbounded metric labels. Show source IP/title
only in local status, never tokens or signed URLs. Keep receiver inventory and
arbitration out of cloud APIs; external-source availability reporting is deferred.

A pinned source retains a dashed connection line while disconnected or buffering,
becoming solid when its amplifier link is active. Explain paused queues, waiting
for audio/connections, and disabled automatic switching in the source's text status;
paused state takes precedence over stale decoder loading status. Mark connected
unselected sources as available. The sender setup uses PulseAudio Zeroconf and PulseAudio Fixed tabs without an outer panel.
Multicast RTP appears when reception is enabled, with copyable laptop sender
per-user PipeWire-Pulse startup configuration and copyable removal commands, with RTP and SAP unbound from any interface (they follow the current route) and marked nofail; its laptop output is named Multicast RTP [group address] because it sends to a group, not an individual speaker. AirPlay appears when
`-raop-listen` is set, with a note when encryption is off. Buffering and initial track loading appear in the source card.

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
tunnel IPs occupy their own line. The output box uses the selected card's USB
manufacturer/product strings when available, otherwise the ALSA PCM name, with
its /dev node below. Resolve descriptors through that ALSA card's sysfs ancestry,
never the global USB inventory; omit serial numbers and generic USB/OS labels.
A bounded startup board-model lookup supplies the heading subtitle.
Source cards label their protocol like the output's eyebrow.
The Slopify card title beside its padlock is the configured server hostname,
without URL credentials, path or query; do not repeat it in the details. Link it
to the configured HTTP(S) origin (preserving port, stripping credentials/path/query),
with target=_blank and rel=noopener noreferrer. Link clicks must not pin the source.
Source drag and arrow ordering stays available in Off, Recent and Priority;
only Priority gives ordering an automatic selection meaning.
Sender instructions include per-user and system-wide persistent Pulse zeroconf
discovery with copy buttons. System-wide configuration applies to each user
session; it does not create a system audio daemon.

The local page heading and browser title use SLOPIFY_NAME (hostname fallback).
Keep the heading sticky while scrolling; measure its height so the sticky
amplifier and scroll targets stay below it, including wrapped names.
Hide the connected-status message but retain failure feedback. Use HiFiBerry
capitalization in display names while preserving lowercase system IDs.

`playback.go` separates queue orchestration from backend/session lifecycle. The
default `native` engine (`native.go`) decodes with `player/internal/audiodec`: integer-only
FLAC, WAV and Q28 MP3 (`mp3tables_gen.go` generates its tables, including integer
requantization powers at build time). MP3 transforms reuse symmetric sums before
rounding; preserve exact output against the direct-transform reference tests. These decoders must not use
floating point; compare against ffmpeg in its tests. Ogg Vorbis (`vorbis.go`, via the pure-Go
`jfreymuth/oggvorbis`) is the deliberate exception: lossy with float reference maths and no
exact output, so it decodes in float32, rounds and saturates once to S32, and is tested
against ffmpeg with a tolerance. Mono/stereo only (surround order is not remapped); its seek
scans page headers from the start. Ogg Opus/FLAC are unsupported. MP3 has no bit-exact definition,
so its accuracy is a measured tolerance; intensity stereo and mixed blocks have no
vectors yet. Output is `player/internal/alsa` (pure-Go ioctls, hw: devices, no conversion,
exclusive use) or null. Mixer controls use the same package, never amixer. Verify with
`SLOPIFY_TEST_ALSA_LOOPBACK=Loopback` on an idle snd-aloop card, and
`SLOPIFY_TEST_ALSA_CONTROL=card:Name`, which only rewrites current values.

The player has no PipeWire client, and the speaker runs no PipeWire. Don't restore it.
`player/internal/raop` is an opt-in AirPlay 1 receiver (`-raop-listen`): RTSP, ALAC over RTP/UDP, audio only;
CLI flags and code say RAOP, the UI says AirPlay. Keep `Stream` and the packet cipher as the seams AirPlay 2
would wrap; it, FairPlay and timed playout are not implemented. Defer AirPlay 2
until AirPlay 1 is usable with updated macOS and iOS clients. No
Shairport or other external daemon. Crypto stays behind `-raop-key`; the RSA key must never be committed or
shipped. `LoadKey` accepts only the key whose modulus matches the public key senders use, and OpenWrt
downloads it from shairport-sync only when both `raop` and `raop_crypto` are enabled. Advertise UDP only:
PipeWire 1.6.2's TCP framing is broken. PipeWire packets exceed the announced 352 frames and omit the
end tag; keep accepting both. `audiodec.ALAC` is integer-only, container-free and checked against ffmpeg.
Track text comes from DMAP (`md=0`; no artwork). DACP remote control applies only while that sender is the
selected source, replacing queue transport for buttons, `/api/playback` and the card; dial the resolved
`iTunes_Ctrl_` endpoint only if it is the RTSP peer's address, and never log or expose Active-Remote.
`player/internal/pulse` is a
playback-only Pulse native-protocol server with no resampling, mixing, DSP or floating-point maths; float32 from clients such as Chrome is unpacked with integer operations.
`internal/esound` is an opt-in playback-only ESound server (`-esound-listen`), also
unauthenticated: 8/16-bit mono/stereo streams only, paced by TCP backpressure, with no
samples, recording or monitoring. ESound has been tried with Windows XP. `internal/upnp` plus `player/upnp_source.go` is an opt-in UPnP AV renderer (`-upnp-listen`)
for controllers such as swyh-rs: SSDP replies, AVTransport, RenderingControl and
ConnectionManager, without eventing or NOTIFY. It pulls FLAC, WAV/RF64 or raw L16/L24 over
plain HTTP; dial-time checks keep streams on private addresses, redirects are refused and
only the host is logged. `liveStream` lets all these protocols share source cards.
`internal/rtp` is an opt-in multicast receiver (`-rtp-receive`) for L16/L24 sessions announced over SAP, also
unauthenticated. A card is one sender IP on one group; its key excludes the display name,
and different sender IPs on the same group remain individually selectable; the port stays out of its identity because PulseAudio randomises it.
It must never block on the output: `pcmring.WriteLatest` discards the oldest audio on overflow, as drift cannot be resampled away.
RTP multicast reception and transmission are tested and working; do not describe
their validation as automated tests only or reception as untested with a real sender.
`rtp.Sender` (`-rtp-send`, off by default) multicasts decoded Slopify audio, never blocking local playback. A random group is picked at each
start unless `-rtp-send-group` is set. OpenWrt generates and persists an explicit
239.192.x.y:5004 destination in UCI on first boot; LuCI validates overrides.
Laptop persistent setup hashes a Slopify-specific prefix plus /etc/machine-id
into 239.192.0.0/16. Native PipeWire RTP/SAP modules keep SAP on 224.0.0.56
while audio uses the derived group. The UI uses one native PipeWire setup because module-rtp-send couples SAP and
audio destinations. Never bind local.ifname in the generated file: a laptop
interface recorded at setup vanishes on undock, and a mandatory module failing
SO_BINDTODEVICE kills PipeWire-Pulse. Both modules carry `flags = [ nofail ]`. The output column lists individual
RTCP receivers with a solid Slopify connection for recent packet reception.
Idle/stale receivers have no connection line. The stream settings remain in a details
block. Reports use RTP port + 1, randomized 2.5–7.5 second intervals, SR/RR plus
SDES CNAME/NAME. Source IP and report-block SSRC isolate senders sharing a group.
Names are untrusted text; cap inventory at 32, expire after 90 seconds and never
send inventory to cloud APIs. Packet reception does not establish audible playback.
`-rtp-local-delay-ms` adds 0–2000 ms before local queue playback while multicast
transmission is enabled (default 200, zero disables); OpenWrt exposes
`rtp_local_delay_ms` and the local multicast details show the configured value.
A bounded, separately queued local copy follows the sender timeline in short writes;
the decoder queue remains 32 blocks. Re-anchor at track boundaries and after
interruptions, with RTP markers allowing receiver correction. This is best-effort,
adds to device latency, and never resamples or edits samples for clock drift.
Control IO runs outside audio callbacks and retries failures without stopping audio.
Do not connect a LAN source to these receivers: multicast currently copies only Slopify queue playback. SAP carries title/artist/album in clear text; never add paths, URLs or internal entry IDs.
The local multicast sender offers `/multicast.sdp`, an SDP snapshot of its current
announced format, plus VLC/mpv commands and M3U/PLS downloads referencing that URL.
No SDP is available before first playback. Reopen after format changes; downloaded
SDP files do not update. VLC requires its Live555 module and supports L16/L24,
not the private L32 extension.
Windows XP SP3 can use 32-bit VLC 3; bundled Windows Media Player cannot receive
this PCM/RTP stream.

The RTP marker on a track's first packet is the receiver's point to correct drift. `internal/discovery` advertises/browses `_slopify._tcp` through the Go D-Bus client and Avahi
when the local UI binds a non-loopback address. Self exclusion uses a random
process ID, never a device token. Bound resolved records to 64, deduplicate URLs,
prefer validated .local names and advertised ports, and clear records on browser
restart. Never fetch discovered endpoints. Sibling links below the title use
textContent and target=_blank with noopener noreferrer. Discovery reveals no
queue identity, signed URL or credential and performs no leader election.
Keep the Pulse start threshold reachable (below target minus minReq): otherwise a stream
stalls after an underflow. PipeWire tunnels answer an underflow by rewinding and
resending, heard as looping, so never read a live stream faster than real time
(`nativeFillPacer`). Reset its deadline and startup guard on every device reopen,
including a source returning after idle. Pulse timing replies report actual
device delay; the protocol indices already account for the stream ring, so do
not add that ring a second time. Never buffer beyond what a tunnel asks. Keep
LAN-source device default (decoded live audio) at 40–80 ms with 10–20 ms periods,
so receiver queues fit within the usual 200 ms PipeWire tunnel target. Slopify
queue originals, compressed and fetched over the internet, keep the deep
200–500 ms default; this discrimination is deliberate. The stall drone's
low-water mark is 100 ms or half the device capacity, whichever is smaller; the
fill pacer reads ahead only below 100 ms or the device start threshold (buffer
minus one period), so a shallow buffer is never permanently "short" and a
device recovering from an underrun refills instead of waiting at its threshold.
`-output-buffer-ms` overrides ALSA output capacity for all sources, including
standalone file playback (40–10000 ms). Zero keeps the respective automatic
defaults. OpenWrt exposes `output_buffer_ms` in the Sinks tab. Input and download
buffers remain separate; do not add per-protocol output knobs. LuCI separates
Slopify server/queue sharing (Sources), ALSA/HAT and multicast transmission
(Sinks), and incoming LAN protocols with their input buffers (Receivers).
The local Pulse card's `syncOffsetMs` is receiver-ring plus device delay, not
measured lip-sync error or sender/network latency. Render changing input bitrate
and A/V offset after the fixed PCM/state text in that row.
Source buffer rows show both underflow and overflow event counts, including zero,
directly from stream statistics even with Prometheus disabled. Keep overflow
units as drop events across receivers and multicast output, never discarded bytes.
Pulse flushes must rewind the write index to the read index when queued audio is
discarded, and timing replies must report actual playing/underrun intervals in
PCM bytes. Permanently reporting a zero playing interval misleads libpulse's
clock smoother once device delay is reported. Track receiver control events
with the bounded `slopify_audio_stream_events_total{event}` family and local
status; those are observed protocol events, not sender-internal DLL corrections.
`nativeRouter` owns the device: only the selected source writes, others keep real-time
pace and stay metered. Sender keys are stable across addresses, so slots carry over.
Hardware level changes move the slider of only the sender connected to the amp; a
sender already at that level is left alone so that a drag never fights its echo.
Each card shows its own "Buffer" at its bottom. `-metrics` defaults off on MIPS
and on elsewhere; disabling it skips instrumentation, TCP sampling/wrappers and
the `/metrics` handler. OpenWrt UCI `metrics` is empty for automatic, `0`/`1` for an
override. Keep source meters, buffering status, arbitration and damage logs active.
Player build metadata is cached at startup from `runtime/debug.ReadBuildInfo`
plus linker `buildVersion`, `buildTime` and optional `buildRevision` values.
`slopify_player_build_info` and local status/footer share it. Keep build time
separate from commit time; unavailable values are unknown, never startup time.
Speaker `/metrics` (`native_metrics.go`)
labels audio series `stage` (download/input/output), `transport` (https/http for queue
originals, multicast, pulse, airplay, esound, upnp, cue) and `source`: the SLOPIFY_SERVER
hostname for the queue, or the verified peer IP (`@group` for multicast), never
sender-supplied names. `slopify_tcp_retransmits_total` is upload loss only; download loss
appears only as out-of-order packets. A slot taken over by another sender deletes
the previous source's series (`noteSource`), keeping series bounded by slots. Add new
measurements to those shared families (`underruns`, `overflows`, `errors`, `buffer_ms`...)
rather than per-protocol metric names. Multicast sender counters join them through an
unchecked collector, so their help text must match the vectors'. Peak windows are stamped on the audio timeline and released
once heard; the chart fills adjoining windows and leaves real gaps visible.

`slopify-player validate` and `play` (`player/filecmd.go`) run the native decoders on files
without a server: validate lists only DAMAGED/FAIL files (`-v` adds clean ones) with each
damaged section's time, byte offset and length; play writes through the normal ALSA path
(or null). Keep them; use them to separate damaged originals from decoder bugs.
Decoders report damage through `audiodec.Options.Report` (`Issue`: resync or truncated). FLAC
resynchronises on the next frame whose CRC-8 and CRC-16 match and fills the gap with exactly
the silence its frame numbers imply, so duration and seeking stay correct; its seek bisects
over intact frames rather than trusting the library's frame index. MP3 checks optional frame
CRC-16s (header + side info) and conceals failures as silence, but only after one CRC in the
file has matched, because some encoders write CRCs that never do; it also reports bytes skipped
between frames. Vorbis lets oggvorbis drop a page failing its CRC-32 and decode on; the lost
length is unknown (Position only realigns at EOF), so nothing is filled and no seek is made:
its seek rescans page headers from the start. WAV reports a data chunk longer
than the file. Transport failures must never count as damage.
`player/media_issues.go` is the one place that logs damage (library path only, never the
signed URL; once per path/kind/offset) and counts `slopify_audio_media_issues_total{codec,kind}`.

The player has one built-in Go decoding/ALSA playback path, without mpv, an
external codec fallback or a playback-engine selector. Unsupported formats fail
visibly. CLI help names PulseAudio/PipeWire together and avoids redundant engine labels.
The main player contains no IR-specific code, flags or amplifier-transmit API.
Generic Linux media keys remain supported. OpenWrt receiver setup lives in the
separate `player/tools/ir` executable (`slopify-ir`): inventory, keymap/eBPF setup,
and bounded scancode learning. This is device setup, not playback. Image Builder
and the manual source workflow compile it separately and install it for LuCI.
No IR transmitter or external ALSA-control daemon is implemented.


The player publishes its receivers itself through Avahi's D-Bus API (`advertise`, `discovery.Publish`):
Pulse as SLOPIFY_NAME plus " via PulseAudio" for instance and description, with `device=` the sink name,
and AirPlay as `HWADDR@name via AirPlay`, so desktop sink lists distinguish transports. Nothing is published for loopback listeners, and an IPv4 listener is
advertised for IPv4 only. Do not restore avahi-publish-service units or the avahi-utils package.
LAN receivers share `liveStream`, `pcmring.Stats`, `cleantext.Line` for untrusted names, one coalescing
sender-volume worker (`sender_volume.go`) and, for senders that take transport commands, `remoteSource`.
Future receivers such as Spotify should implement those rather than add parallel paths.
Chromecast is excluded from the roadmap because of its receiver authentication requirements.
Spotify remains unimplemented; track upstream playback failures at
[librespot #1649](https://github.com/librespot-org/librespot/issues/1649).
LAN authentication is deferred; preserve the current trusted-LAN boundary.
Multicast retransmission is limited to `multicastSources`: Slopify and the planned Spotify backend. `player/internal/receiverconfig` shares the `SLOPIFY_PULSE_ENABLED`
deployment flag for the player setup UI.
Discovery is not authentication. Keep manual IP
tunnel instructions for routed networks; do not configure multicast reflection.

Source selection and queue state remain distinct. The local source-pause worker
coalesces selection intents independently of routing. Request a conditional
pause only when selection transitions away from Slopify, never on each later
Play, track change, or restored startup selection. Failed mutations are not
replayed against newer playback decisions. Its cloud pause command is `pause-if-sole-output`; the server checks selected membership
and playback revision under its queue lock. Multiple selected outputs keep
playing (including this device's paced, unheard stream). Automatic selection of Slopify
never resumes a paused queue; an explicit pin sends `play-if-sole-output`, under the same checks. If detached after output expiry, an explicit Slopify pin rejoins only this speaker to its owner’s queue, preserving other outputs and ongoing playback, just like the physical play/pause button. The card shows “Not joined to queue” instead of a stale paused state. No receiver inventory goes to the server.

Everything specific to the device player lives in `player/`: the main package, its
`internal/` packages, `server/deploy/`, `desktop/` and `openwrt/`. Server images never copy it.
Only `internal/httpx` and `internal/volume` are shared with the servers.

The Pi and Cubietruck OpenWrt profiles include onboard Wi-Fi drivers, board firmware and WPA/LuCI
support. Keep Pi `disable-wifi` absent and preserve configured radios in network
defaults; fresh images have no Wi-Fi credentials. Ethernet remains directly on
eth0; configure station Wi-Fi on a separate DHCP interface rather than restoring
br-lan. Cubietruck needs BCM43362 firmware and its sunxi base-files calibration;
Pi 3/3B+ need the matching BCM43430/43455 firmware and NVRAM packages.

Cubietruck stock OpenWrt 25.12.6 exposes its onboard IR receiver after
installing kmod-sunxi-ir and kmod-input-evdev, but ir-keytable reports no
supported/enabled protocols, rc-empty and no LIRC node. The configured feeds
have no protocol decoder packages. Do not claim functional remote reception
from the driver/input node alone; decoding still needs kernel support.

Cubietruck S/PDIF playback works with its driver installed. The generated image
manifests confirm `kmod-sound-soc-sunxi-spdif` is included for its digital output;
`kmod-sound-soc-sunxi` alone exposes only the analog codec. The installed
6.12.112 S/PDIF driver currently advertises only S16_LE; do not weaken lossless
format negotiation to hide this. The user-authorized `-allow-truncate` option
explicitly permits S16_LE as a final fallback after precision-preserving formats,
without resampling, rounding or dithering. It is off by default, including
OpenWrt `slopify.player.allow_truncate=0`; a device override must not become an
image default. Log fallback and report the actual S16_LE output format. Future kernels need the upstream MSB-alignment
and 24-bit/DMA/format-mask fixes recorded in the OpenWrt README, with on-hardware
precision verification. Both fixes first shipped in Linux 6.14 and are absent
from the OpenWrt 25.12.6 / Linux 6.12.112 kernel in use. S32_LE acceptance alone
does not prove 32-bit S/PDIF.

Pi/Cubietruck OpenWrt defaults bind LAN directly to `eth0` and remove the stock
`br-lan`. GL-AR750/AR750S profiles instead retain an L2 bridge, merge all three
wired sockets into LAN, disable IP forwarding and use DHCP/DHCPv6 client
addressing. Wi-Fi station setup remains optional; never assume an ordinary
station interface transparently bridges the upstream LAN.

`appliance/` builds Raspberry Pi 3/4/5, Cubietruck, GL-AR750 and GL-AR750S images with the official Image
Builder, adding a static player compiled outside it through `FILES=`. It runs as nobody with group audio. SSH is Dropbear (LuCI SSH-Keys, Ed25519/sk-ed25519/RSA only, no ECDSA; build-time keys disable password login),
plus ttyd/luci-app-ttyd running /bin/login on LAN. Keep it free of
SDK builds and custom packages; images have no mpv; originals use FLAC, WAV, MP3 and (except soft-float MIPS) Vorbis. The LuCI
panel is plain files under `files/`. Raspberry Pi HAT selection uses Lua helpers
in `files/usr/libexec/slopify/`: firmware EEPROM Auto by default, Off, or installed
audio overlays. Preserve existing explicit settings, require reboot, and refuse
complex boot declarations rather than rewriting them. Amp4 needs
`hifiberry-dacplus-std` when generic DAC+ misdetects Pro clocking; `-list-audio` gives it cards and volume controls. OpenWrt init grants audio-group read access to all event devices and passes their
expanded paths through repeatable -input flags; standard media keys need no
source selector. Input hotplug coalesces changes for one second and restarts
only a running player. The separate Rotary tab/-rotary-input opts one device
into relative-axis volume; other relative events, including mice, are ignored.
Supported release devices are Raspberry Pi 3/4/5, without GPIO IR receive.
Cubietruck and GL-AR750/AR750S are manual experimental targets only: do not build
or publish them in release CI. Keep manual Cubietruck and AR750S instructions.
`build.sh` defaults to only the three Pi profiles.
`.github/workflows/release.yml` runs on version tags (`vMAJOR.MINOR.PATCH`, with
optional prerelease suffix) and pushes to main. Branch builds stop before
registry and GitHub Release publication and use a development chart version.
It calls `openwrt.yml` for only those three Pi
profiles and `player.yml` for static AMD64, x86, ARM64, ARMv7/v6 and soft-float
MIPS/MIPSLE players. Six AMD64 server images go to GHCR; inline workflow shell uses
the build action's digest outputs directly, copying only chart metadata,
default values and templates, never private installation values. The OCI chart
is `oci://ghcr.io/<owner>/charts/slopify`; GitHub Release assets include its
archive, release values, players, Pi images/manifests and SHA256SUMS. Publish the
GitHub Release only after every build succeeds. Release CI compiles Go
on the runner and invokes the official `openwrt/imagebuilder` Docker images;
never run the experimental source/SDK kernel workflow there. Pi profiles share
`profiles/rpi-common.sh` and use bcm2710/bcm2711/bcm2712 with their own upstream
kernels/firmware. Pi 4/5 still need hardware verification. LuCI checks kernel RC
class availability and ir-keytable through audio-info: if absent, gray the IR
receive tab and disable mutations while preserving stored mappings and a
clickable explanation. Document the Amp4 custom-kernel/BPF/BCM26 setup separately;
Image Builder alone cannot add missing kernel IR features.

`appliance/source-build.sh` is an explicit, experimental source-build
workflow for ARM64 Pi rpi-3 and ARMv7 Cubietruck, separate from the default official
Image Builder. It pins OpenWrt 25.12.6 and its feeds, builds with an unprivileged
Debian container, restores the pinned upstream board defaults and uses
OpenWrt KernelPackage definitions for module settings, retaining non-RT scheduling,
and packages GPIO IR input/output, RC-core/LIRC, NEC/RC5/RC6 through the local
source-only `kmod-slopify-ir` package. `v4l-utils` provides IR/keymap tools.
RC-core must be built-in (CONFIG_RC_CORE=y), with CONFIG_BPF_LIRC_MODE2=y;
general BPF support alone cannot attach IR decoders. The v4l-utils feed patch
enables upstream BPF decoding. Keep protocol decoding out of the player.
Preserve these requirements and check the generated config, not just the
requested fragment. Keep upstream CPU/toolchain/optimization defaults; the
user dropped Pi 4/5 and cross-family shared-userspace requirements. Do not
patch their CPU_TYPE or switch subtargets in this workflow. OpenWrt selects
one subtarget per build configuration. Source builds must set umask 022
and normalize tracked OpenWrt/feed checkout modes before package assembly:
host umask 027 otherwise ships /sbin as 0750 and prevents the ubus user
from executing ubusd. Keep private untracked keys and installed shadow modes
restricted. Validate standard directory/account modes in the assembled root. `userspace.sha256` records the Pi 3
userspace ELF hashes.
PREEMPT_RT was dropped at the user’s request. Require PREEMPT_NONE and reject
PREEMPT_RT in generated configs. The previous Pi 3B+ RT image is NOT deployment-ready: hardware boots crashed
and reset, with USB/network lost. It retains CONFIG_USB_DWCOTG=y and DWC2=n;
investigate the legacy driver RT path and obtain the full panic trace before
claiming a fix. Do not infer the cause solely from fw-clock or mld_ifc_work
messages. Pi 4/5 remain hardware-unverified. Keep build/config validation
separate from boot/playback/IR validation. Preserve IR with a non-RT kernel
for recovery if needed; never mix a stock kernel with mismatched RT modules. Build state is disk-backed `.build-tmp/openwrt-rpi3`
(`SOURCE_WORKDIR`), results `dist/openwrt-ir` (`OUT`); never use /tmp or
/dev/shm, prune caches or flash a board as part of a build. GPIO overlays remain
opt-in. Grant all input event devices audio-group read access and the
configured LIRC transmitter read/write access at startup; input hotplug
restarts the running player. Hardware boot/playback/IR verification remains separate; these features
are not supplied by the default Image Builder. The explicit Cubietruck IR test
build uses its own `.build-tmp/openwrt-cubietruck` checkout, sunxi/cortexa7 board
defaults and kmod-sunxi-ir for the onboard receiver, plus the same built-in
RC-core/LIRC/eBPF and NEC/RC5/RC6 decoder bundle. Validate CONFIG_IR_SUNXI=m,
sunxi-cir.ko and pulse_distance.o in the assembled image. Require one profile per
checkout; never switch the existing Pi build to sunxi. Keep Cubietruck outside
release CI. Its S/PDIF precision fixes remain deferred. See the OpenWrt README.

The OpenWrt IR receive tab configures kernel-decoded remote input. There is no
IR transmit tab or amplifier code. `player/tools/ir` supplies bounded
`ir-info` inventory, root-only `ir-learn`, and `ir-setup` via init: validate
fixed media actions and protocol-bounded scancodes (48 bits for Denon), use a private temporary TOML and a five-second
`ir-keytable` deadline, and select a receiver by physical identity rather than
rc index. All event nodes receive audio-group read access on OpenWrt. Repeatable `-input`
selects devices using fixed standard media keys, with repeat allowed for volume
only. `-rotary-input` separately opts one device into REL_X volume. Expose carrier filter fields only when
LIRC advertises the relevant capability; GPIO demodulator frequency is fixed
by hardware. Never invent a map for an unverified remote model, enable GPIO
pins automatically, decode IR in the player or add a listener for setup.

`player/deploy/` is the supported Ubuntu 26.04 Pi install: `overlay/` mirrors the root
filesystem and `install.sh` installs it with the player. Keep this directory free of
private addresses and device names. The prohibition on committing `server/deploy/raspi` remains.

`player/docs/design/local-network-audio.md` describes the implemented LAN audio design and, in a
separate section, proposals for AirPlay, Android, Windows and authentication. Those proposals
are not authorization to expose new LAN ports. Keep the local receiver boundary separate
from cloud service ingress; `server/deploy/raspi` remains an uncommitted experiment.

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
- Sidecar `credits` hold well-known tags only (composer, copyright, license,
  label, ISRC, AcoustID and MusicBrainz IDs; probe version 3/video 5). Keys fold
  case, spaces, underscores and hyphens across Vorbis, ID3 TXXX and MP4 freeform
  spellings. Text is capped at 512 bytes; identifiers are dropped unless they
  validate (lowercase UUID, 12-character ISRC). Catalog keeps credits out of
  browse/search, returning them only from `/api/catalog/item`; Nerd mode links
  validated IDs to musicbrainz.org/acoustid.org. Arbitrary tags are not stored.
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

- OIDC by default, with Passmower `OIDCClient`; no local accounts or passwords.
  Explicit `SLOPIFY_ANONYMOUS=true` on frontend/catalog/players uses the fixed
  `slopify:anonymous` issuer and `anonymous` subject, one shared queue and no
  OIDC discovery. Anonymous mutations require `X-Slopify-Request: 1`, reject
  cross-site fetches and mismatched Origin; never derive identity from headers.
  Use separate player state from OIDC installations. `compose.yaml` is the local
  Linux host-network anonymous stack, public `[::]:8080` (dual stack), backend
  loopback ports 18081–18088 and metrics 19090. It is a local-machine example,
  configured directly through private `server/.env` values; run Compose from
  `server/` so it loads that file automatically. There is no Go launcher.
  Only indexer
  mounts those media directories writable. The existing private
  `player/compose.yaml` is the private separate speaker setup; run it from
  `player/`. Keep both private files gitignored and out of the repository root.
  Compose bind paths resolve relative to `server/compose.yaml`, so its nginx
  mount uses `./deploy/compose/nginx.conf`; disable automatic host-path creation
  to fail on missing configuration files. The private player setup builds
  with `player/Dockerfile` and the repository root as its context.
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
  ReportedName is device-local identity and the web Name, without an owner prefix
  or web rename. Old NameSet overrides are cleared when refreshing device names.
  Persist verified OwnerIssuer/OwnerSubject, strip both from public JSON. Migrate
  legacy devices only from a unique selecting user queue; otherwise require pairing.
  Current queue keys remain subject-only under one configured issuer. Do not add
  multiple issuers or change issuer without an explicit account-state migration.
  Cross-user output sharing belongs to federation; do not restore the removed
  LAN takeover flow. Same-queue multicast reuse grants no cross-user access.
  Federation and separate-IdP grants are proposed in `server/docs/design/federation.md`;
  matching emails/names never link identities or confer speaker/library access.
- Signed media URLs grant temporary access without OIDC cookies. Do not log
  their signatures, embed bearer tokens in exports or trust request Host as
  the canonical origin. PUBLIC_ORIGIN comes from the OIDC_CLIENT_ORIGIN secret
  for absolute playlist URLs; API audiences expand that origin plus /api.
- M3U and PLS exports snapshot the entire present queue and always use signed
  originals, valid for 12 hours. They do not synchronize mpv/VLC with Slopify.

## Queue, browser and device playback

Playback position is telemetry, never a seek command. Explicit seeks carry an
immutable `seek` event (`id`, `entry`, `position`) in queue/output snapshots,
including private browser-output views. Clients apply each event once and use
its target, independently of report revisions and extrapolated position. Copy
the event with playback state; progress reports must neither recreate nor
change it. Clear it when starting another entry. Preserve gapless sessions
through delayed track acknowledgments; no version/position drift heuristic.
A session latches the newest eight heard completion IDs before reporting them.
Snapshots naming those completed entries cannot rewind an active session while
the queue command is unchanged; explicit commands and seek events still apply.

- One durable logical user queue per OIDC subject; browsers and Linux
  devices are replaceable outputs. The queue follows the account across tabs
  and phones. Keep ownership checks on commands and browser reports.
- Each browser is one output: its tabs share the ID in localStorage, and a Web
  Lock (`slopify-output-<id>`) makes one tab play it. The newest tab steals the
  lock; older tabs become controllers with "Use here", and Play in a tab claims
  it. While music plays, the old tab keeps sounding until the new one reports
  playing over BroadcastChannel (8 s limit); a new tab blocked by autoplay hands
  back instead of reporting blocked. Inactive tabs never register the output,
  report or follow its desired state. Without Web Locks tabs keep separate
  sessionStorage outputs. To keep a second screen playing, use a web sink link.
- Preserve the current entry separately from the remaining queue. Clearing
  the queue or removing its playing item must not interrupt playback.
- Users can select multiple outputs. One primary output advances the queue.
  This is shared control, not clock- or sample-synchronized multiroom audio.
- Selected outputs are pins that lapse (`server/cmd/players/pins.go`): once the queue
  has been stopped for an output's `outputKeep` minutes (default 30, 0 = always)
  it is deselected, so a later Play cannot wake a sleeping room. Nothing lapses
  while playing; choosing outputs or a timeout restarts `idleSince`. Lapse is
  checked before any command that starts playback and swept every 30 s. Web
  commands carry `from` (the browser output): if every pin lapsed, that browser
  is selected. Opening a tab never moves playback between devices or other
  browsers. Explicit Play may still add this browser when every selected output
  is an offline browser; never steal from an available output.
- GET/HEAD requests retry transient proxy errors (502/503/504) and network
  failures across redeploys. Auth-token renewal is explicitly retryable; queue
  mutations and link creation are never replayed automatically. Cancel obsolete
  library reads and refresh the library after SSE reconnect. Cached app assets
  cover transient frontend errors; codec fallback must distinguish proxy outages.
- Browser periodic progress reports run only while the document is visible;
  foregrounding sends a fresh position. Playback events (including ended and
  lock-screen controls) still report while hidden. This is not a guarantee
  against mobile OS suspension or SSE disconnects.
- Device event-stream disconnects retain reachability and playing state for
  `deviceReconnectGrace` (30 seconds). Reconnect cancels the generation-guarded
  timer; expiry clears reachability and pauses only if still playing. Explicit
  pause/source-switch commands bypass grace and retain their own pause reason.
- Browser disconnects have a 30-second grace period before pausing the output;
  a closing tab's keepalive `gone` report shortens it to 5 s (a reload still
  reconnects). An installed app (`display-mode: standalone`) registers with
  `background=1`: on disconnect while playing it stays reachable for the rest of
  its track plus a minute (10 min if unknown, at most 4 h), renewed by each
  report, then pauses. Report replies carry the output's state so its own
  "ended" report delivers the next track without SSE.
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
- Devices always fetch originals and play supported audio formats only. Unsupported
  codecs and video containers fail visibly. No video rendering in the Go device player.
- Preserve device sample rate and decoded precision. No resampling, software
  volume or lossy conversion. Lossless wider-word packing and mono duplication
  are allowed to match hardware. Unsupported sample rates are errors.
  Use ALSA `hw:` outputs to avoid implicit ALSA conversion.
- Device events, reports and media share one HTTP/2 connection. HTTP/2 pings
  (15 s idle, 5 s timeout) and a 60 s event-stream read watchdog detect dead paths,
  such as after a router restart; TCP alone takes about 15 minutes. The watchdog
  must exceed the server's 20-second device keepalive. Socket behaviour is per
  connection through `-tcp-*` flags (`outbound_tcp.go`), never sysctls, and apply
  to every backend using the shared transport. `TCP_INFO` sampling every five
  seconds feeds `slopify_tcp_*` metrics; download loss appears as out-of-order
  packets, since retransmits count only what the speaker sent. The `host` label
  is the dialled name: CDN domains collapse to wildcards such as `*.spotifycdn.com`,
  others cap at 16 values then `other`; never paths or URLs. Multicast receive retries
  in the background rather than exiting when the network is not up yet. A FLAC
  seek at or past the last sample ends the track rather than failing playback.
- Prefetch the next track for gapless device playback; reopen the DAC only on
  format changes and release it while paused. The native engine (`range.go`) downloads
  the current original to its end in the background, then starts the next one; both
  share one `-read-ahead-mib` budget (default half of min(RAM, cgroup memory.max), at
  most 1 GiB), and a set GOMEMLIMIT is raised by it. Preserve detached current entries
  in device queue views when the visible queue is cleared.
- Device hardware volume is opt-in with `-volume-control` and `-volume-card`;
  without flags, never read or set mixers. `-volume-min`, `-volume-max` and
  `-volume-initial` use alsamixer's perceptual displayed 0–100 scale (defaults
  0, 40, 10), not raw hardware-register levels. Map levels to hardware using
  ALSA dB metadata; 20/40 on Amp2 correspond to roughly -42/-24 dB.
  Set the initial level once per process start; poll local mixer changes each
  second and enforce bounds. Advertise capabilities/dB steps on device SSE
  connection and report observed levels, hardware dB and mute through `/device/v1/volume`. The players
  service publishes them over SSE. Web volume readouts and accessible slider
  text show only dB (−∞ dB for mute), never ALSA level numbers/control names.
  Volume commands require the output's current account selection and use independent revisions; never alter playback versions
  or queue state. Linux repeatable `-input` consumes explicitly selected evdev devices using
  fixed media keys; -rotary-input separately enables REL_X volume. OpenWrt passes
  all current event devices and restarts on input hotplug.
  Bare Amp2 encoder wiring: BCM23/24 rotation, BCM25 push, commons to GND;
  rotary-encoder/gpio-key boot overlays provide pull-ups. Ubuntu uses stable udev symlinks for its configured inputs; OpenWrt grants
  audio-group read access to all event nodes. Device `/device/v1/control` permits
  only version/entry-checked play/pause, next and previous of the queue selecting that device.
  A deselected device's play/pause re-selects it in its owner's queue and plays
  (joining, never pausing, playback elsewhere); it can add only itself.
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
  in `server/deploy/storageclass-transcodes.yaml`, outside the app chart.
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
- `slopify_indexer_tagged_files{mimetype,tag}` is the same kind of gauge for
  well-known tags: `tag` is a fixed list (browsing fields title…disc plus credit
  fields such as composer, isrc, musicbrainz_recording); never tag keys or values
  from files. Files not yet re-probed at the current version count as untagged.
  Coverage: `slopify_indexer_tagged_files / ignoring(tag) group_left sum without(audio,video) (slopify_indexer_files)`.
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
  in OIDCClient templates. Empty lists fail rendering unless `allowAllUsers=true` explicitly admits every
  OIDC account; authentication remains required.
- Chart requires Passmower OIDCClient and Mittwald StringSecret controllers.
  Generated signing/session keys and queue state are persistent. Media may
  use hostPath or a PVC; only indexer mounts it read-write.
- Per-service Dockerfiles in `server/build/` cover cluster services, with explicit
  input copies to avoid unrelated rebuilds. `.dockerignore` allows only build
  inputs. Services without ffmpeg use static Go binaries in scratch.
  Indexer/transcoder use Alpine plus ffmpeg. Do not restore a generic root
  Dockerfile. `player/Dockerfile` is the exception: it builds the device player, which normally runs under systemd or procd, and has no mpv.
- `go.mod` targets Go 1.27.1.
- Experimental, uncommitted Pi service/configuration files live in `server/deploy/raspi/`: a generic
  systemd unit, explicit Amp2 volume/rotary drop-in, udev input permissions,
  headless boot snippet/configurator and static Ethernet example. Installer
  restarts only the player; boot and network changes are separate, never an
  automatic reboot. Keep real server/IP settings in gitignored
  `server/deploy/raspi/local/`; never version pairing tokens or compiled player binaries.
  Do not commit this experiment; the supported install is `player/deploy`.
- Demo: `server/deploy/demo.sh` explicitly targets the public ee-lte-1 demo namespace
  and development registry (amd64 build and scheduling), then runs Skaffold's `demo` profile. That profile
  uses `chart/demo.yaml` and private `chart/values-demo.yaml`, never local values.
  The demo's `persistentWorkloads` switches media/state services to one-replica
  StatefulSets (required by this k3s cluster's storage admission), disables
  transcode disk caches, and opts into `allowAllUsers`. OIDC and per-user ownership
  remain enforced. Demo queue state uses `/data/demo-players.json`, preserving
  the incompatible legacy state on its PVC. `server/cmd/demoseed` is shipped in the indexer image and runs as an
  init container in the indexer Pod with UID/GID 1000, sharing the media PVC.
  Initialization finishes before the first scan; inotify remains the only ongoing
  change detection. Do not add periodic rescans for the demo. Its embedded manifest pins sizes,
  checksums, provenance and licenses; unique hidden partials are atomically published without replacement after
  verification. Three download workers bound parallelism. It never overwrites existing media. The import stays
  inside the indexer Pod and never writes sidecars.
- Legacy `server/deploy/build.sh` uses Kaniko in `$KUBE_CONTEXT`/`$NAMESPACE` and
  `chart/values-demo.yaml`. It uploads context as a ConfigMap, avoiding
  `kubectl exec`. Partial builds change only selected deployment images.
  `server/deploy/seed.yaml` seeds sample media.
  Browser playback mechanics have focused Node tests in server/web/media.test.mjs;
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
| `SLOPIFY_TOKEN_FILE` or `SLOPIFY_TOKEN` | device | persistent token file or explicit token |

Use `SLOPIFY_OUTPUT=null` for a device without audio hardware. Server health
checks must not wait for an initial library scan. Keep deployment specifics
current in values files rather than adding hard-coded locations to services.

OpenWrt replaces `/etc/banner` with Slopify branding and uses
`/etc/profile.d/50-slopify.sh` plus the `motd` helper for live player status,
audio card, web/LuCI URLs and service/log commands. Suppress the stock package-manager cheatsheet with its .hush marker; keep
stock profile safety warnings intact. Laptop multicast lives in `30-slopify-rtp.conf`, independently
of unicast/discovery drop-ins; do not replace those files. Expose native RTP directly as an Audio/Sink, avoiding the asynchronous
module-null-sink startup that can skip later pulse.cmd drop-ins on PipeWire 1.0.5.
Lock multicast output volume/mute at unity using `channelmix.lock-volumes=true`;
application gain and individual speaker hardware volume remain independent.


OpenWrt's player defaults to HTTP :80, LuCI to :8080; the one-time
92-slopify-web-ports migration preserves custom bindings and LuCI HTTPS. Use
procd's capability JSON (CAP_NET_BIND_SERVICE only) and procd-ujail while retaining
nobody:audio. Never lower the system-wide privileged-port threshold or run the
player as root to bind port 80. Receiver-only pages omit the dormant Slopify card;
reordering their visible LAN cards must preserve that hidden slot's position.


Priority's higher-source guard is `priorityTakeoverMs` (0–10000, default 0),
independent of `prioritySilenceMs` and Recent's `minimumSoundMs`. Require fresh
continuous above-threshold observations, reset by silence or meter gaps. Apply it
to upward automatic selection, including the idle Slopify fallback path; manual
pins bypass it. Persist `transitionSounds` (default true). The native router
serializes a 220 ms integer-synthesized, configurable source-change cue between
closing the old output and opening the next. Do not meter, multicast or mix it;
respect hardware mute, skip unchanged selections and startup, and release its
output before normal writes. Cue failure must not prevent the source switch.
`-cue-level-db` sets peak dBFS (integer −60..0, default −6), with fixed-point
attenuation; the stall drone is 6 dB lower. OpenWrt passes `cue_level_db`.
The main notes are 440/660 Hz. Initial Play, Resume and a queue track selection
that restarts the decoder use an ascending cue, repeating once per second until
the first decoded sample chunk, cancellation or failure. Hold at most two chunks
before normal output begins; cues stay outside position, meters and RTP.
The same setting also gates a 220 ms descending pause cue after closing a live
queue session on a playing-to-paused transition. Skip repeated state, startup,
finished sessions and other selected sources; serialize it with source switching.
It is local only, never metered or multicast. The same setting gates two other
native-engine cues on the Slopify output: a 120 ms
upward sweep when a seek restarts playback (played at once, in the last format),
and, once a track is playing on a buffered device, a looping winding-down drone
(6 dB below the configured cue level) written whenever nothing is decoded and the device holds under
its low-water mark (100 ms, or half a shallower buffer). Gate the drone on at least one second of music written in the current
track, and check cancellation before every fill; startup and cancelled sessions
must not add a winding-down tone. Neither is metered, multicast or counted in the reported position.

Sibling discovery uses github.com/godbus/dbus/v5 directly with typed Avahi
EntryGroup/ServiceBrowser/ResolveService calls, two bounded resolver workers and
a bounded signal queue. Reconnect and clear the cache on daemon/bus restart or
signal overflow. Do not restore command-line publishing/browsing for siblings.

The local title's settings cogwheel takes `-local-settings-url` and optional
`-local-settings-port`; resolve relative paths against the current browser URL
and allow only credential-free HTTP(S). OpenWrt passes the Slopify LuCI path and
the configured uhttpd HTTP port, preserving the browser's device hostname.

Queue deduplication is opt-in via `-queue-share` / OpenWrt `queue_share`.
`queue_link.go` owns bounded peer gossip on the existing `/api/queue-link`
listener; only private numeric Avahi endpoints are dialled, without redirects
or proxies. Four workers probe at one-second intervals with 800 ms deadlines;
peers expire after three seconds. The players service persists a random
QueueGroup on the canonical user queue, exposes it only to selected devices,
and rotates QueueCommand on sendQueue. Never expose owner identity, signed URLs
or credentials in LAN gossip. Election is local: explicit RTP senders precede
other eligible nodes, then process IDs break ties. Eligibility requires playing
Slopify selected locally. Followers keep their logical Slopify selection and
cloud SSE/control session, stop native decoding, and route the matching sender
instance/IP/group underneath it. Fresh RTP plus a peer claiming leadership is
required before following; source switches, seeks, missing audio or peer loss
return to direct decoding. Follower reports use its own version/entry and only
matching hashed track tags; preserve the server's primary-output semantics.
Queue inputs are excluded from automatic source selection while aliased; explicit
pins still win. Show the automatic input at 20% opacity, normal opacity for a
direct pin. Draw one solid source-coloured line at 20% opacity directly from
that multicast input to the amplifier; omit the duplicate Slopify output line.
Keep the upstream name on the Slopify card. An
implicitly created RTP sender sends audio only while elected with another
listener; explicitly configured senders retain their independent behaviour.

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
link. Diagnostics remain beside their sources/output. Escape or clicking outside
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
Images also ship `prometheus-node-exporter-lua` (thermal, netstat, openwrt,
filesystem) on LAN :9100 via `93-slopify-node-exporter`, which only replaces the
stock loopback binding. Pi images add `kmod-hwmon-raspberrypi`; our
`hwmon_alarm.lua` collector exports `node_hwmon_alarm` (in0_lcrit is the current,
non-sticky under-voltage flag) because the stock collector skips alarms.
`player/monitoring/` is a laptop Prometheus (compose with inline config); its
host-side `refresh-targets.py` resolves `.local` names into gitignored file_sd
targets, since containers cannot use mDNS. Never commit speaker names or IPs.

The native GNOME Shell extension in `desktop/` uses GJS, Avahi D-Bus
and MPRIS, with no Python service or build step. Its dropdown contains metadata,
hardware volume slider, transport and source/player selection; native OSD names
the speaker and dB. Media keys are captured only in desktop/overview modes, with
volume keys skipping adjacent levels with identical hardware gain to avoid
quantization stalls. Key handling uses
persisted backup and restore on disable or Laptop controls. Local
`GET/POST /api/playback` exposes bounded queue metadata and version/entry-checked
transport through the controller owner, retaining same-origin JSON checks and
device-scoped cloud authorization without retries. See [the extension guide](desktop/README.md).

The OpenWrt `cpufreq.lua` collector exports scaling current/min/max frequency
in Hz and governor info per CPUFreq policy, using sysfs and omitting unavailable
values. It is read-only; a policy may cover several CPUs.

`hwmon_alarm.lua` also exports sampled rising-edge counters as
`node_hwmon_alarm_events_total`. Serialize observation and updates with a
nonblocking advisory lock across uhttpd scrape processes, retain missing states,
and atomically persist only in `/tmp` (no flash writes). Reboot resets counts;
brief alarms between scrapes remain unobserved.

OpenWrt `slopify-cpu` starts before the player and pins each CPUFreq policy's
scaling min/max to its own `cpuinfo_min_freq`, choosing powersave when available.
UCI `slopify.cpu.minimum` defaults to 1, including upgrades without the section;
set it to 0 and reboot to restore kernel defaults. Do not hard-code a Pi clock
for other boards or change firmware voltages. The CPUFreq exporter is read-only.

Queue pause attribution is an immutable name/source/time snapshot, copied to
outputs with playback state and cleared on resume/stop. Device commands distinguish
physical buttons, local controls and source selection; stale commands must not
rewrite attribution. The web shows the pausing name with source/time in a tooltip.
Input devices use fixed Linux media bindings. To make a GPIO button mute,
configure KEY_MUTE (113) in its gpio-key overlay. OpenWrt consumes standard media keys from all event devices, has a separate
Rotary selector for relative-axis volume and per-action Learn/Clear rows under
IR receive, with no separate Controls tab. Learning reads full LIRC scancodes under an exclusive evdev grab
for at most five seconds, waits for release/silence, and never changes keymaps.
Empty IR maps are allowed so the receiver/protocol can be enabled before
learning. Mute stays local; the web output list labels muted outputs.

Queue topology is also exchanged in bounded SLQ1 RTCP APP packets on the existing
RTP control groups. Only private source IPv4 addresses become peer endpoints;
validate the advertisement and reject older topology revisions. Emit topology
changes promptly and retain normal randomized periodic reports. RTCP freshness
is 20 seconds (two missed maximum report intervals); HTTP-only freshness is
three seconds. Keep at most 32 known endpoints for 90 seconds and poll them
without depending on the current Avahi snapshot. Explicit eligibility/following
withdrawals immediately affect election and listener demand, independent of
background RTP reception. No credentials or media URLs enter RTCP.

Native codec opt-out tags are `no_flac`, `no_wav`, `no_mp3`, `no_vorbis`,
`no_alac`. Disabled factories return ErrUnsupported with a build-time explanation;
raw PCM remains available. MIPS soft-float feature tags automatically omit Vorbis.
Keep codec-specific imports/tables behind the same constraints, including generated
MP3 tables. `player/internal/linuxioctl` translates generic read/write directions
into the MIPS ABI; ALSA and LIRC share it. Linux control structs use native byte
order; PCM payload byte order remains explicit. GL router image profiles use
GOMIPS=softfloat and smaller UCI read-ahead/memory targets for their 128 MB RAM.
`gl-ar750s` means the official ath79/nand NOR/NAND profile. The build copies
sysupgrade binaries, initramfs kernels, manifests and checksum metadata alongside
SD-card outputs. No router hardware playback has been verified yet.
