# Slopify server

The server hosts a personal music and video library, browser playback and shared
queues. Use its browser UI from a desktop or phone to browse, search, add tracks
and choose browser or dedicated speaker outputs.

## Goals and features

- A responsive browser experience for large libraries.
- Queues and playback state that survive changes of browser or speaker.
- Original-file streaming, optional browser transcodes and direct device playback.
- OIDC sign-in, account-owned queues and private browser receiver links.
- Separate container services for indexing, catalog, streaming, players,
  frontend and transcoding.

## Deploy

The Helm deployment expects an HTTPS ingress, Passmower's `OIDCClient` controller
and Mittwald's `StringSecret` controller. Configure allowed accounts, domain,
media storage and persistent player state in private installation values.

From the repository root, install a published release with:

```sh
helm upgrade --install slopify oci://ghcr.io/codemowers/charts/slopify \
  --version VERSION --namespace slopify --create-namespace \
  -f chart/values-local.yaml
```

`VERSION` is the release version without its leading `v`. The published chart
pins all six AMD64 server images by digest. See the
[deployment guide](docs/operations.md#deploy-the-server) for prerequisites
and storage settings.

For development, copy `skaffold.env.example` to `skaffold.env` and
`chart/values-local.example.yaml` to `chart/values-local.yaml`,
customize them, and run `skaffold dev` from the repository root. Private settings
are gitignored. The `demo` profile has separate private demo values.

[compose.yaml](compose.yaml) is a trusted-LAN local-machine example. Its usage and
required environment variables are in the [deployment guide](docs/operations.md).

See [architecture and implementation notes](docs/operations.md) and
[repository guidance](../AGENTS.md) for service responsibilities and API behavior.
