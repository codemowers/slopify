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
| players | `cmd/players` | Per-user queues, output selection, pairing and playback state; persists `/data/players.json`. No media mount. |
| frontend | `cmd/frontend`, `web/` | Static UI and OIDC login/token renewal. No API proxy or BFF. |
| transcoder | `cmd/transcoder` | Per-profile HTTP workers fetching signed originals. No media mount. |
| device player | `cmd/player` | Fetches originals, decodes audio with ffmpeg, plays through ALSA. |

The gateway was removed. Do not restore forwarded-user-header authentication or
route all APIs through frontend. An enricher and S3 backend are future work,
not implemented components.

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
- Browser disconnects have a 30-second grace period before pausing the output.
  Retry transient network errors from the last played position, including
  Chrome's ERR_NETWORK_CHANGED; do not treat network failure as user pause.
- Library + always appends to the queue tail. The append command ignores
  insertion hints; use insert for drag/drop placement and next-up for Play next.
- Library rows have explicit add/play actions. Search toolbar defaults to the
  results; selecting a row targets that row. Cap additions at 300 tracks and
  deduplicate search results by path. Folder navigation uses breadcrumbs and
  recursive folder additions, not a nested directory tree.
- Device/output selection, browser audio quality, appearance and nerd mode
  live in settings. Nerd mode is a third desktop column, stacked on narrow
  screens. Show source audio/video codecs in file tooltips.
  Dock video below the scrolling library on desktop, above playback controls
  on mobile. Preview has no duplicate title/transport/header buttons; clicking
  it opens native fullscreen controls. Output selection is top-right in the
  page header. Selected offline outputs stay visible, grayed out, and sync to
  current queue state on return. Queue rows use selection and per-row
  actions without checkboxes; coarse-pointer taps toggle selection.
- Browser video tries originals when its audio codec is supported; rejected
  containers can fall back to MP4 with both codecs copied (`video-copy`).
  Unsupported audio uses stereo AAC 192 kbps (`video-aac-192`). Never transcode
  video. Source video codec support remains a browser/hardware limitation.
  HEVC in MP4 is tagged `hvc1` when source metadata identifies HEVC.
- Transcoded/remuxed video uses fragmented MP4. Seeking opens a fresh stream
  with audio preroll aligned to a source video keyframe; it is approximate.
  Original streams use ordinary HTTP range seeking. Keep errors visible.
- Devices always fetch originals, including video containers, and decode only
  audio with `-map 0:a:0 -vn`. No video rendering in the Go device player.
- Preserve device sample rate and decoded precision. No resampling, software
  volume or lossy conversion. Lossless wider-word packing and mono duplication
  are allowed to match hardware. Unsupported sample rates are errors.
  Use ALSA `hw:` outputs to avoid implicit ALSA conversion.
- Prefetch the next track for gapless device playback; reopen the DAC only on
  format changes and release it while paused. Preserve detached current entries
  in device queue views when the visible queue is cleared.

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
- Per-service Dockerfiles in `build/` have explicit input copies to avoid
  unrelated rebuilds. `.dockerignore` allows only build inputs. Cluster services
  without ffmpeg use static Go binaries in scratch. Indexer/transcoder use
  Alpine plus ffmpeg; native player target currently uses Debian plus ffmpeg
  and ALSA utilities. The documented Docker run enforces a read-only
  root filesystem, with a writable token volume. All builders use the same
  per-service Dockerfiles in build/. Do not restore a generic root Dockerfile.
- Demo: `deploy/build.sh` uses Kaniko in `$KUBE_CONTEXT`/`$NAMESPACE` and
  `chart/values-demo.yaml`. It uploads context as a ConfigMap, avoiding
  `kubectl exec`. Partial builds change only selected deployment images.
  `deploy/seed.yaml` seeds sample media; `deploy/testplayers` has null outputs.
  Browser playback mechanics have focused Node tests in web/media.test.mjs;
  do not recreate legacy tests that forge gateway cookies or mount session keys.
- No ready-made Pi/Cubietruck SD-card images are built. OpenWrt is a possible
  appliance base, not an implemented distribution. Persistent pairing tokens
  would need `/etc` rather than volatile `/var` there.

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
| `SLOPIFY_SERVER`, `SLOPIFY_NAME`, `SLOPIFY_OUTPUT` | device | required URL, hostname, `aplay:hw:0,0` |
| `SLOPIFY_TOKEN_FILE` or `SLOPIFY_TOKEN` | device | persistent token file or explicit token |

Use `SLOPIFY_OUTPUT=null` for a device without audio hardware. Server health
checks must not wait for an initial library scan. Keep deployment specifics
current in values files rather than adding hard-coded locations to services.
