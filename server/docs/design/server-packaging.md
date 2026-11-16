# Server packaging and optional OpenWrt hosting

Status: planned, not implemented. Keep the current deployments and working
Pi/Cubietruck player installations unchanged while preparing this work.

## Direction

Introduce one `slopify-server` executable with `frontend`, `catalog`, `players`,
`streamer`, `indexer` and `transcoder` subcommands. First use it in the existing
container deployment, preserving separate processes, services and HTTP/SSE APIs.
Keep `slopify-player` a separate executable and deployment.

Extract reusable component implementations from `server/cmd/` into Go packages with
explicit configuration and lifecycle management. Entry points stay thin. Avoid
global HTTP muxes, implicit environment reads inside components and process exits
from library code. Do not introduce a second server implementation for OpenWrt.

Build two container variants:

- Core: the shared server executable and CA certificates.
- Media tools: the same executable plus ffmpeg for indexer/transcoder.

Sharing the executable can eliminate repeated runtime/library code on disk;
measure the result rather than assuming a size or RAM saving. A shared image
also broadens rebuild and rollout scope when any server component changes.
Preserve the Skaffold workflow and assess that tradeoff during implementation.

## Privilege boundaries

Linking code together does not require running it together. Each subcommand
continues to run in its own container or procd jail:

- Indexer: UID/GID 1000 and writable media for sidecars; never modify media files.
- Catalog and streamer: read-only media access.
- Players: writable queue state, no media access.
- Frontend: authentication secrets and web UI, no media access.
- Transcoder: signed-source fetching and its own cache, no media mount.

Goroutines share process credentials, filesystem access, memory and failure
domains. They cannot preserve these boundaries. A combined server process is
not part of the initial plan. In particular, keep the writable indexer and
audio player isolated.

## Optional Raspberry Pi server

Provide independently configured LuCI entries named **Slopify Server** and
**Slopify Player**. One Pi may run both; player-only images omit the server
executable. Launch server subcommands independently under procd with the
appropriate access restrictions.

Do not ship ffprobe/ffmpeg in the Pi image for basic USB music indexing. Add
native metadata probing, initially for FLAC, MP3 and WAV, reusing appropriate
parsing code while keeping device-specific audio code out of server packages.
Existing decoders are only part of the solution: tags, artwork, duration and
container probing need explicit coverage. Report unsupported files clearly.
Keep ffprobe as an optional backend for wider format and video support on
full server installations. Native probing is separate work from binary linking.

Store media and persistent server state on USB storage rather than OpenWrt's
writable flash. Wait for the configured mount when it is absent; do not index
the empty mountpoint or create state in its underlying filesystem. Handle
removal without interpreting it as deletion of the entire library.

Authentication remains an open design decision. The current server requires
OIDC; self-contained offline USB use needs an explicitly designed local mode.
This plan does not authorize bypassing authentication or exposing new services.

## Alternatives considered

1. Separate binaries for all components: current model, narrow rebuilds and
   dependencies, but repeated linked code.
2. Shared server binary plus separate player: selected direction, retaining
   process isolation while simplifying server artifact distribution.
3. One binary including server and player: possible with subcommands, but
   couples updates and makes player-only devices carry server code unless
   reduced builds are maintained. Not selected.

## Implementation sequence

1. Extract component entry points and add the server subcommand dispatcher.
2. Build and measure the shared binary and both container variants. Verify
   existing API behavior, security contexts, mounts and Skaffold behavior.
3. Add native music metadata probing with reference fixtures and explicit
   unsupported-format behavior.
4. Resolve local authentication, USB mount lifecycle and resource limits.
5. Add optional Pi server packaging, procd services and the separate LuCI page.
   Verify player-only and server-plus-player installations on hardware.

For context, locally cached images inspected during planning were approximately
11.8 MB frontend, 12 MB catalog, 12.1 MB players, 10.4 MB streamer, 210 MB indexer
and 205 MB transcoder (Docker uncompressed sizes, not download sizes or additive
disk usage). Indexer and transcoder currently install Alpine's full ffmpeg
package. Re-measure when implementing; these are not shared-binary results.
