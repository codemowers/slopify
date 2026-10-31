# User-owned outputs and federation

## Implemented boundary

Each installation currently verifies tokens from one configured OIDC issuer and
its own API audience. Hardware pairing claims a device for the authenticated
user's queue, atomically. Other users cannot see a claimed device, select it,
rename it, forget it or change its volume. Pending devices remain visible to
signed-in users and the first successful claim wins; this is not a physical
possession challenge. Keep installation sign-in restricted accordingly.

A device reports its local name (hostname by default, or its configured name).
The default web label is `<owner display name>'s <reported name>`, for example
`Lauri's HiFiBerry Amp2`. A web override replaces that entire label without
changing the device's local identity or discovery name. Clearing the override
restores the generated label. Verified display claims are presentation, never
permission keys. Raw owner issuer/subject and device credentials are omitted
from public player JSON.

Previously adopted devices migrate only when exactly one saved user queue
selects them. Otherwise they require pairing again. The owner display identity
is filled from verified claims on the owner's next authenticated visit.

Sharing, multi-server attachment and federation below are proposals. Current
queue IDs still hash the subject within a single configured issuer namespace;
they are not federated identifiers. Changing an installation's issuer requires
an explicit identity/state migration. Do not enable multiple issuers against
that subject-only queue namespace.

## Separate identity providers

Federation must not require a shared IdP. Treat local OIDC identity as the exact
verified `(issuer, subject)` pair. Equal subjects, emails, usernames or display
names from two issuers do not establish equal identity. Even accounts at the
same IdP need not have matching subject identifiers for different clients.

Keep three identities distinct:

- User identity: issuer and subject, or a server-local opaque account ID bound
  to that pair. Explicit account links are optional and revocable.
- Server identity: a configured federation peer and its verified signing keys.
  Key rotation must preserve that peer's durable identity through an authorised
  rotation; do not identify peers from arbitrary request Host headers.
- Resource identity: origin server plus opaque library item, queue or output ID.
  A path alone is never a globally unique track reference.

Use an explicit grant flow. Suppose Lauri is logged into server A via IdP A
and wants music from server B, whose users sign in through IdP B:

1. A creates a bounded, expiring authorisation transaction tied to Lauri's local
   session and the intended B peer. The browser visits B.
2. B authenticates the user through B's IdP. The user approves named permissions
   for A: for example browse a collection and play its originals. B's owner may
   instead issue an invitation with those rights if the guest has no B account;
   an invitation must explicitly bind the accepted remote principal.
3. B returns a single-use code bound to A, the exact callback and transaction.
   A exchanges it over authenticated HTTPS with proof binding the exchange to
   the initiating transaction. No access token appears in a redirect URL.
4. B issues a restricted, expiring delegation for A, with audience B's API,
   allowed resources/actions, grant ID and revocation state. A stores it on the
   server, scoped to the initiating local account. A's OIDC token is never
   forwarded to B, and B's login/refresh tokens are not handed to A.

This flow delegates resource access; it need not merge accounts or make A trust
IdP B directly. An optional account-link workflow requires authentication to
both accounts and must not silently combine permissions or transfer speakers.
A person without a B account needs an explicit guest invitation or B-admin
provisioning; OIDC cannot manufacture authorisation.

Do not assume either IdP supports OAuth token exchange. Implement the Slopify
resource grant boundary independently of IdP-specific extensions. Signing-key
trust alone never grants access: B checks the intended audience, authorised
peer, acting principal, scope, resource and revocation. Reject substitution of
an ID token for an API or delegation token.

## Remote browsing and mixed-origin queues

A keeps the user's durable queue and presents authorised local and remote
catalogs together. Each queued remote item carries origin peer, opaque item ID
and a metadata snapshot. B remains authoritative for current access and media.
Do not forward arbitrary remote URLs or accept filesystem paths as authority.
Bound remote searches, pagination, response sizes, concurrency and timeouts.
Restrict peer destinations and redirects to prevent SSRF, including DNS changes.
Peer reachability needs explicit deployment egress rules.

When an item is played, A uses its delegation to request a short-lived stream
grant from B. The receiver gets only that item's temporary media URL, not a
catalog credential or either user's login token. Browser playback and hardware
playback use the same resource/grant model. Browser direct playback additionally
needs B's explicit CORS policy for approved origins and the required media/range
requests; alternatively an authorised A-side media relay can hide that boundary
at a bandwidth cost. Remote transcoding must be scoped as well as originals.

Keep unavailable remote entries visible with a useful error and a defined retry
or skip policy. Never reset a whole queue because one peer is down. Revoking a
delegation stops new browsing/signing; already issued media URLs remain usable
until expiry unless B checks revocation while serving them. Use short lifetimes
and state the actual revocation guarantee in the UI.

## Owned speakers and browser outputs

Library grants and output grants are separate. Access to a collection does not
permit changing a speaker's volume, and access to a speaker does not reveal its
owner's library. Both hardware and browser outputs can be shared through an
explicit output grant with an expiry and revocation, with proposed roles:

- Play: submit authorised playback from an allowed queue.
- Control: select sources, pin/unpin and control volume within owner limits.
- Admin: manage sharing and configuration; ownership transfer is a separate act.

One authoritative controller serialises each output's active queue lease. Each
command carries a lease generation; stale commands from an old server cannot
steal playback after a switch. The owner can revoke a lease or grant. A speaker
may keep outbound connections to several servers using distinct credentials,
but only the current lease holder drives playback. Browser outputs require the
same arbitration, reconnect and stale-command rules. Existing read-only shared
browser output links remain a separate capability, not federation credentials.

The local reported name remains useful for mDNS. A remote controller displays
an owner-approved label and the owning server where disambiguation is needed;
neither discovery names nor sender IP addresses authenticate an owner.

## Suggested implementation order

1. Finish user ownership and names on the current single-issuer installation.
2. Introduce issuer-qualified account keys for queues, browser outputs and all
   authorisation paths, with an explicit one-issuer legacy-state migration.
3. Add resource grants, peer configuration, revocation and tests for two IdPs
   deliberately issuing identical subject strings. Test substituted audiences,
   replayed codes and revoked grants too.
4. Add remote catalog browsing and mixed-origin queues for browser playback.
5. Apply output grants and queue leases to hardware and browser output sharing.

Federation is therefore more than token validation. OIDC supplies local login;
Slopify must supply cross-server consent, constrained delegation, resource
identity and playback ownership.
