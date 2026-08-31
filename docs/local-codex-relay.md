# Native Codex compatibility-layer operations

This is the canonical runbook for connecting a remote OpenCodex deployment to
native Codex CLI, the local Codex AppServer, Codex Desktop, and Linux Remote
Control hosts. The implementation lives in [`../client/relay/`](../client/relay/).
Repository checks pass. External ARM Remote live acceptance is recorded below;
local-client/Desktop/Voice acceptance remains a separate operational step.

Private release records, deployment-specific rollout evidence, dated outcomes,
and incident or rollback records remain in the private deployment overlay and
are deliberately not published with this Core. This runbook is limited to the
generic implementation and validation contract.

## Purpose and boundary

The compatibility layer keeps Codex on its built-in `openai` provider. Only a
loopback relay injects Cloudflare Service Auth and the distinct Nginx gateway
credential.

```text
Codex CLI / local AppServer / Desktop-facing Codex config
  | built-in openai provider
  v
127.0.0.1:18180  opencodex-relay
  | CF-Access-Client-Id
  | CF-Access-Client-Secret
  | X-OpenCodex-API-Key
  v
Cloudflare Access -> Tunnel -> OCI 127.0.0.1:18080 Nginx
  | exact route/method allowlist
  | edge credentials removed
  v
OCI 127.0.0.1:10100 OpenCodex -> selected upstream account
```

The design preserves these invariants:

- Codex threads keep the native `openai` provider identity.
- Native Codex `Authorization` is preserved. Client-supplied admission headers
  are removed and replaced with values from the trusted local credential store.
- Every supported private-IP HTTP external profile requires an explicit
  plaintext acknowledgement because native Authorization and request content
  remain visible without TLS; Cloudflare Access remains HTTPS-only.
- The relay and central OpenCodex/Nginx listeners remain loopback-only.
- Dashboard, management APIs, and arbitrary `/v1/*` paths are not published.
- Credential values never enter Codex TOML, launchd/systemd definitions, the
  repository, or relay logs.
- The current gateway key is shared device admission, not per-device RBAC or a
  quota boundary.

### Prebuilt macOS app onboarding

A downloaded, ad-hoc-signed app does not require the end user to supply source,
package, output, or manifest-signing inputs. When launched outside an Applications
folder, Control Center locks server fields and offers **Move to Applications**.
The app stages and verifies a copy, prefers `/Applications/OpenCodexRelay.app`,
asks before falling back to `~/Applications/OpenCodexRelay.app`, and does not use
`sudo`, Authorization Services, Terminal, quarantine removal, or a Gatekeeper
bypass. A different existing destination is replaced only after confirmation and
is retained as a sibling backup until the new instance completes a nonce-bound
startup handshake. The unchanged source is kept by default; moving it to the
macOS Trash is a separate explicit choice. Ambiguous recovery never deletes a
bundle.

After relocation and the user's normal Privacy & Security approval, Settings
enables only the server address, authentication profile, and required credentials.
Left-clicking the menu bar icon opens the compact status popover; right-clicking
offers Control Center, refresh, Login Item Settings, and quit only. Routing,
recovery, and removal actions stay in Control Center.

## Design decision and the OpenCodex pattern reused

Upstream OpenCodex normally runs its proxy on `localhost:10100` beside Codex,
then injects and synchronizes native Codex configuration and the model catalog.
Because a long-lived AppServer can retain an old in-memory list after the file
changes, OpenCodex also has an explicit, narrowly matched AppServer restart
path. This implementation reuses these contracts:

- Codex remains the Responses API client and owns its local AppServer.
- `openai_base_url` and startup-loaded `model_catalog_json` are the native
  integration points.
- Catalog replacement is validated and atomic, while long-lived AppServer
  activation is handled as a separate lifecycle event.
- CLI, AppServer, and Desktop must be proven to read the same `CODEX_HOME`.

A remote deployment cannot reuse OpenCodex's shared-loopback assumption. Each
client therefore runs a small relay that terminates central admission
credentials locally. The public data plane never exposes the central
Dashboard, management API, or a remote AppServer transport.

| Alternative | Benefit | Decision for this deployment |
| --- | --- | --- |
| Direct custom provider to the central gateway | Fewer local files | Legacy rollback only. It changes provider identity and couples admission secrets to the Codex process environment |
| Publish central OpenCodex directly to clients | No local daemon | Rejected. It removes the credential-injection, exact-route, and Desktop same-home boundaries |
| SSH local forwarding only | Simple and strong for administration/recovery | Retained for Dashboard/OAuth management; unsuitable for an always-on client data plane and automatic catalog delivery |
| Run full OpenCodex on every client | Closest to the upstream local form | Not selected because it duplicates the central account pool and operational authority |
| Built-in `openai` plus loopback relay | Preserves native provider identity and local product features while isolating admission secrets | Selected |

The Codex configuration reference likewise directs a built-in OpenAI provider
at a proxy/router with `openai_base_url` and defines `model_catalog_json` as a
startup-loaded catalog. AppServer and Remote Control have their own native
process lifecycles and are not treated as the Responses proxy itself.

## Native-surface compatibility

| Surface | Behavior | Current evidence |
| --- | --- | --- |
| Codex CLI Responses and tools | Uses relay through `openai_base_url` in the selected Codex config | ARM external and x86 local-relay paths passed a four-turn Responses/tool scenario; Desktop acceptance remains separate |
| Local Codex AppServer | Uses relay when it reads the same Codex home/config | External ARM Remote daemon, catalog reader, and proxy handshake live-checked; verify each local process separately |
| Codex Desktop project/session | Uses relay when its local AppServer reads that Codex home | Configuration path implemented; live Desktop acceptance not run |
| Linux Remote Control | Uses a dedicated `CODEX_HOME` and the routing-mode catalog owner | ARM external and x86 local-relay Remote paths are live-verified |
| OpenAI-compatible image API | Forwards `/v1/images/generations` and `/v1/images/edits` | Allowlist implemented |
| Desktop-native image UI | Not intercepted when it uses a separate product control plane | Native path retained; outside relay validation |
| GPT-Live/Realtime compatible routes | Forwarded only with local and central opt-in | Routes implemented; audio/WebRTC not live-tested |
| MCP, plugins, and local tools | Execution and permission ownership are not rewritten | Native behavior retained |

A CLI success does not prove Desktop, Desktop-native image, or Voice behavior.
Those surfaces can use a different Codex home or a separate product control
plane and need independent acceptance evidence.

## Allowed API contract

The local relay and central Nginx allow only these exact routes.

| Capability | Method and path | Condition |
| --- | --- | --- |
| Models | `GET`, `OPTIONS /v1/models` | Also used for catalog refresh |
| Responses | `POST`, `OPTIONS /v1/responses` | HTTP/SSE |
| Responses WebSocket | `GET /v1/responses` | Requires `Upgrade: websocket` |
| Compact | `POST`, `OPTIONS /v1/responses/compact` | Shares only the coarse gateway safety ceiling; OpenCodex owns turn admission |
| Images | `POST`, `OPTIONS /v1/images/generations`, `/v1/images/edits` | Compatible API requests only |
| Artifact | `GET /v1/opencodex/artifacts/{opaque-id}` | One slash-free opaque ID |
| Search | `POST`, `OPTIONS /v1/alpha/search` | Compatible OpenCodex extension |
| Live setup | `POST`, `OPTIONS /v1/live`, `/v1/realtime/calls` | Local and central Voice enabled |
| Live sideband | WebSocket `GET /v1/live/{id}`, `/v1/realtime`, `/v1/realtime/calls/{id}` | Local and central Voice enabled |

There is no catch-all proxy. The local relay returns `404
endpoint_not_enabled` outside the contract, and the central gateway returns
`404` outside its exact allowlist.

## Supported release targets

| OS | Architecture | Intended use |
| --- | --- | --- |
| macOS | `arm64` | Primary Apple Silicon user machine |
| Linux | `amd64` | x86_64 Remote/external Codex host |
| Linux | `arm64` | AArch64 Remote/external Codex host |

The release builder produces CGO-free static Go binaries. The central OCI
OpenCodex host architecture is independent of the client relay target.

## Files and credentials

### macOS

| Path/store | Contents | Protection |
| --- | --- | --- |
| Keychain service `opencodex-relay-cf-access-client-id` | Cloudflare client ID | Login Keychain |
| Keychain service `opencodex-relay-cf-access-client-secret` | Cloudflare client secret | Login Keychain |
| Keychain service `opencodex-relay-gateway-api-key` | Nginx gateway key | Login Keychain |
| `~/.config/opencodex-relay/relay.json` | Non-secret relay configuration | `0600` |
| `~/.codex/opencodex-relay-catalog.json` | Verified visible-model catalog | `0600` |
| `~/.codex/config.toml` | Marker-owned `openai_base_url` and `model_catalog_json` | Existing user file preserved |
| `$CODEX_HOME/opencodex-relay-interactive.config.toml` | Explicit side-session profile for the reserved listener; only `openai_base_url` and the same `model_catalog_json` | Marker-owned `0600`; an existing unmarked file blocks installation |
| `~/.local/lib/opencodex-relay/relay/` | Versioned binaries and `current` symlink | User-only |
| `~/Library/LaunchAgents/io.github.novelkr.opencodex-relay.plist` | Secret-free user service | `0600` |

### Linux

Linux uses `~/.config/opencodex-relay/credentials.env` instead of Keychain. The
directory must be `0700`; the owner-only regular file must be `0600`. It accepts
only literal `NAME=value` rows for these names:

```text
CF_ACCESS_CLIENT_ID=REPLACE_WITH_SERVICE_TOKEN_ID
CF_ACCESS_CLIENT_SECRET=REPLACE_WITH_SERVICE_TOKEN_SECRET
OPENCODEX_GATEWAY_API_KEY=REPLACE_WITH_GATEWAY_KEY
```

`export`, command substitution, extra variables, and shell syntax are rejected.
The user service is `~/.config/systemd/user/opencodex-relay.service` and
contains no credential value.

## Build and publish a signed release

The production installer does not trust an arbitrary binary URL. Build and sign
one manifest with an off-repository Ed25519 private key:

```bash
./client/relay/scripts/build-release.sh REPLACE_WITH_VERSION \
  --base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --signing-key /secure/off-repo/opencodex-relay-release-ed25519.pem \
  --previous-build-number REPLACE_WITH_GREATEST_PUBLISHED_CF_BUNDLE_VERSION \
  --output /secure/release-staging/REPLACE_WITH_VERSION
```

Publish the manifest, signature, four Linux helpers, ad-hoc-signed
`OpenCodexRelay.app.zip`, and `THIRD_PARTY_NOTICES.md` together under
`RELEASE_BASE/VERSION/`. Compatibility revision 4 binds each component, the
macOS bundle ID, `signing_mode: "adhoc"`, the final app zip hash, and the notice URL/SHA-256 into
the signed manifest. Distribute the trusted public PEM through an independently
reviewed channel. Never put the private key on the repository, release host, or
client.

The installer verifies the Ed25519 manifest signature, every asset SHA-256,
the zip shape, nested ad-hoc signatures, Hardened Runtime, and absence of a
Relay Team ID before changing `current` to the bundled helpers. It neither
invokes notarization tools nor treats Gatekeeper's first-launch block as a
transaction failure. Linux retains raw helper verification. Revision 4 includes
the privileged helper and generic `OpenCodexRelayHelperInstaller`; the latter
generates the fixed LaunchDaemon only after a separate administrator-approved
`install` or `update`, and its XPC policy binds exact app, installer, and helper
CDHashes. Revision 1 and 2 manifests remain supported for rollback, but
predate the parked routing controller and are not a safe MenuBar-managed
Desktop switch. It
fails closed if an existing same-version directory
differs from the manifest or an existing relay config points at another
upstream. If service activation fails after native routing has been prepared,
it restores the exact prior relay JSON and Codex config, the prior `current`
target, and the prior launchd plist/systemd unit plus its manager state. A
failed first enrollment restores absence of the new JSON, routing block, and
service artifact; the verified version directory may remain on disk but is not
selected or enabled.

The distribution app keeps the complete SemVer in `CFBundleShortVersionString`
and takes its independently increasing `CFBundleVersion` from
`client/relay/RELEASE_BUILD_NUMBER`. The builder accepts only `1...9999` and
requires it to be greater than `--previous-build-number`. The production app
contains the tracked Ed25519 public key with byte-for-byte and fingerprint
verification under `Contents/Resources/ReleaseTrust`. Revision 5 verifiers
strictly bind the channel, minimum updater/macOS versions, trust key ID, and
integration/helper protocols. The first updater bootstrap, `0.3.8-rc.6`, stays
on revision 4 so existing manual installers can consume it; existing users must
install that bootstrap release manually.

### Public GitHub Release distribution

Publish official artifacts from the public Core repository. Enable
[immutable releases](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)
before the first publication. The release tag must equal the build version
exactly: use `1.2.3`, not `v1.2.3`.

The publishing workstation uses a `gh` login with write permission for that
repository; the signing private key remains off-repository and off-client.

```bash
./client/relay/scripts/build-release.sh 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --signing-key /secure/off-repo/opencodex-relay-release-ed25519.pem \
  --previous-build-number REPLACE_WITH_GREATEST_PUBLISHED_CF_BUNDLE_VERSION \
  --output /secure/release-staging/1.2.3

./client/relay/scripts/publish-github-release.sh 1.2.3 \
  --repo OWNER/opencodex-relay-releases \
  --input /secure/release-staging/1.2.3 \
  --public-key /secure/off-repo/opencodex-relay-release-ed25519.pub \
  --release-notes-fragment client/relay/release-notes/1.2.3.md
```

The publisher checks public visibility, an absent version, the manifest
signature, four Linux helper URLs, the signed macOS bundle component, and the
signed notice URL/hash before uploading a draft. It verifies the exact
eight-asset set before publishing and again after
publication. It fails if GitHub does not report an immutable release; do not enroll
clients against such a release. GitHub attestations are supplemental evidence;
the out-of-band public PEM and signed manifest remain the client trust root.

Stable tags publish with `prerelease=false` and `latest=true`; prerelease tags
publish with `prerelease=true` and `latest=false`. The publisher reads back the
exact tag, draft/prerelease/immutable state, eight asset names, and GitHub API
digests. Release API ordering is never treated as SemVer ordering.

Public release downloads require no credential. A separate expiring, read-only
token may be supplied only to avoid anonymous GitHub API rate limits. It is not
an OpenCodex data-plane credential and never enters the relay service or Codex
configuration.

```bash
install -d -m 0700 ~/.config/opencodex-relay
umask 077
${EDITOR:-vi} ~/.config/opencodex-relay/github-release.token
chmod 0600 ~/.config/opencodex-relay/github-release.token

./client/relay/scripts/install-relay.sh install 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

The GitHub consumer path uses the GitHub REST asset API plus `curl` and `jq` to
fetch only exact-tag release assets and then verifies the manifest and target
checksums. If supplied, the optional token file must be a regular,
current-user-owned mode-`0600` file; it is written only to an owner-only
temporary curl config and removed on exit. The generic `--release-base-url`
path remains supported.

### Non-integrating Preview run

Run `./script/build_and_run.sh` from the repository root for Desktop UX review.
It creates a complete helper-bearing app with `OpenCodexRuntimeMode=preview`,
then launches only that app. It does not consume or create a routing binding,
touch launchd, Keychain, `/Library`, or an installed app, and it does not open
Terminal or run `sudo`.

The read-only `./script/build_and_run.sh --integration-preflight` returns 0 for
`ready`, 3 for `integration_required`, and 4 for `unsafe`/`invalid`, while
printing only bounded codes. Actual integration remains the separate reviewed
local-development build/install procedure below.

### Local-only development distribution

For direct person-to-person or organization-internal macOS testing, use the
separate local-only development path. It is **not** a production release and
does not weaken the official signed-manifest revision-4 contract. It supports only
macOS 26+ Apple Silicon and has no release URL, GitHub downloader, automatic
updater, Developer ID, notarization, Gatekeeper assessment, or quarantine
removal.

Build only from a clean, committed source tree. Local manifest schema 3 signs
the exact commit, one ad-hoc-signed `OpenCodexRelay Dev.app.zip`, and notices.
The bundle contains the privileged helper and the same fixed-purpose generic
`OpenCodexRelayHelperInstaller` used by production, under isolated `.dev`
identifiers. Its app metadata pins the exact helper
CDHash used for XPC peer validation. The development installer accepts only
`status --json`, `install`, `update`, `uninstall`, and explicit `recover`; it
derives every source and destination from its containing bundle and fixed
system locations. The root-owned schema-2 transaction journal records bounded
phase and backup witnesses. An interrupted operation remains blocked until
`recover` either verifies the current exact bundle through XPC or restores the
previous byte-exact artifacts and launchd state. Schema 1, malformed, or
incomplete mutation evidence is never inferred. The only pre-backup exception
is a schema-2 `preparing` transaction whose original helper/plist witnesses and
launchd loaded state still match exactly; recovery discards that untouched
preparation without changing the files or service. `backups_ready` and every
later phase remain backup-bound.

```bash
./client/relay/scripts/build-local-dev.sh 1.2.3-dev.1 \
  --signing-key /secure/off-repo/local-dev-ed25519.pem \
  --output /secure/local-transfer/1.2.3-dev.1
```

The direct-transfer default verifies the source directory's public PEM only
after an explicit acknowledgement. This detects accidental damage but assumes
the transfer path itself is trusted. For repeated transfer, first pin a public
key whose fingerprint was obtained separately, then use its Keychain service:

```bash
./client/relay/scripts/install-local-dev.sh trust enroll \
  --keychain-service opencodex-relay-local-dev-trust-example \
  --public-key /secure/separately-verified/local-dev-public-key.pem \
  --expected-fingerprint REPLACE_WITH_LOWERCASE_SHA256

./client/relay/scripts/install-local-dev.sh install 1.2.3-dev.1 \
  --source-dir /secure/local-transfer/1.2.3-dev.1 \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --keychain-service opencodex-relay-local-dev-trust-example \
  --acknowledge-local-development-source
```

Without a Keychain pin, replace `--keychain-service` with
`--acknowledge-local-source`. The dev installer uses only
`~/.local/lib/opencodex-relay/relay-dev`, a separate config/binding, listeners
`127.0.0.1:18190/18192`, and LaunchAgent `io.github.novelkr.opencodex-relay.dev`. It starts
native parked and never changes Codex TOML, catalog, credential access, or
OpenCodex state during install. A dev and production namespace cannot own the
same selected Codex config; use a separate `CODEX_HOME` for parallel testing.
The user must manually launch/approve the local development app created outside
the reviewed release channel. Login registration is
optional from the running app and is never performed by the installer.

`--acknowledge-unsigned-local-build` remains a deprecated compatibility alias.

`--config` is limited to a clean absolute path under
`~/.config/opencodex-relay/relay-dev/`; the installer rejects a symlinked parent
or a path that resolves outside that namespace. Uninstall is fail-closed: it
requires the installed dev helper and two native-ownership proofs (before and
after stopping the dev service). If the config is missing while dev artifacts
remain, preserve them and recover manually rather than deleting the install.

## Install on Apple Silicon macOS

Register all three credentials through prompts that keep values out of shell
history:

```bash
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-cf-access-client-id -w
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-cf-access-client-secret -w
security add-generic-password -U -a "$USER" \
  -s opencodex-relay-gateway-api-key -w
```

For routine Desktop use, prefer **Control Center → Settings → External
Gateway** over these bootstrap commands. The Settings card provides a bounded
address field, **Test connection**, **Apply**, and separate SecureFields for
adding or replacing the Cloudflare Client ID, Cloudflare Client Secret, and
Gateway API Key. It never reads a credential value back into the UI and does
not offer deletion. New items use a login-Keychain ACL limited to the Desktop
app and `/usr/bin/security`; replacement preserves existing ACL entries while
repairing either required trust entry. The app then proves a non-interactive
`/usr/bin/security` read before reporting the save.

The connection test performs one authenticated
`GET /v1/models?client_version=…` using the resident catalog parser. It has a
five-second timeout, no redirects or retries, and an 8 MiB response limit; it
never writes the catalog, pending marker, or config. Apply repeats the test and
requires the config digest and routing generation observed by inspect. While
External is active, Relay parks new admission, drains in-flight requests,
atomically writes the config, and asks the existing RuntimeManager to replace
the External generation; Codex Desktop stays running. Native and Local modes
only persist the address for a later External selection.

Credential writes are deliberately independent of address apply and remain in
Keychain after a failed test. Any credential write invalidates the verification
receipt. The UI distinguishes validation required, credential-combination
mismatch, unreachable, and catalog response error. Receipts store only config
digest, Keychain modification times, verification time, and result code.

For owner-bound diagnostics, pass the candidate only through JSON stdin:

```bash
opencodex-relayctl gateway inspect --json
opencodex-relayctl gateway test --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
opencodex-relayctl gateway apply \
  --expected-config-digest REPLACE_WITH_INSPECTED_SHA256 \
  --expected-routing-generation REPLACE_WITH_INSPECTED_GENERATION \
  --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
```

The apply refuses a pending/recovery state or any config, routing-generation,
or credential race. A live-swap failure restores the previous config and
runtime. If Relay cannot prove that rollback, it remains fail-closed in
`recovery_required`.

Install an explicit signed version:

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

Add `--migrate-legacy` for the documented root provider `pw_opencodex`,
`opencodex`, or `pw_opencodex_remote`. The former direct loopback base URL
`http://127.0.0.1:10100/v1` or `http://localhost:10100/v1` is also explicitly
supported. Migration creates a timestamped mode-`0600` backup, removes only
those known root assignments and `model_catalog_json`, and preserves provider
tables. Other custom providers and arbitrary `openai_base_url` values are
rejected for manual review.

Verify without printing a secret:

```bash
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
curl --fail --silent http://127.0.0.1:18180/__relay/healthz
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl catalog refresh
codex debug models
```

The manual refresh command never fetches, writes, activates, or restarts an
AppServer. It reports that a relay-owned catalog is owned by the resident
lifecycle, or that a `remote_manager` catalog is owned by that manager;
`--no-apply` remains accepted only for backward CLI compatibility.

For revision 4, inspect `mode status --json` rather than the removed
`codex_routing=true` shorthand. A previously active profile reports
`phase=relay_active`, `relay_admission=allow`, and `catalog_refresh=run`. A
fresh macOS External enrollment intentionally reports
`phase=relay_pending_restart`, `relay_admission=deny`, and
`catalog_refresh=pause` until the user selects Desktop, lets it quit, and
applies the request. A changed catalog leaves its `.restart-pending` marker;
restart the same-home CLI/Desktop rather than broadly killing AppServers.
Validate a real Responses call in a new CLI process and confirm Desktop reads
the same Codex home.

### Switch modes and inspect connection state

On macOS 26+ Apple Silicon, revision 4 installs the ad-hoc-signed, Hardened
Runtime `~/Applications/OpenCodexRelay.app` link. It does not launch the app or
require login registration as part of installation.
The MenuBar app stores and controls only a Codex Desktop `.app` whose reviewed
bundle ID, strict code-signature signed identifier, and exact ten-character
uppercase Apple Team ID all match; it revalidates that identity immediately
before every quit or relaunch boundary. A user-selected path, display name, or
running process is not trust evidence.

On 2026-08-23, the installed Codex Desktop `26.818.41509` at
`/Applications/ChatGPT.app` was independently checked with
`codesign --verify --deep --strict`, its designated requirement, and Gatekeeper's
`Notarized Developer ID` assessment. That review established the
`com.openai.codex` / `2DC432GLL2` tuple. Production and local-development plists
and both build/install paths pin and verify the exact tuple. A future signer
identity change fails closed until a new official signed build is independently
reviewed and the tuple is explicitly updated; a selected path never supplies
trust-on-first-use evidence.

Desktop lifecycle control uses neither AppleScript nor force quit. The current
preserve-only removal path does not use Finder Apple Events or move user data.
Because the Relay app is not notarized, open it from Finder once. If macOS
blocks it, immediately open **System Settings → Privacy & Security**, choose
**Open Anyway** for OpenCodexRelay (normally available for about one hour after
the blocked launch), and confirm **Open**. Updates may require this approval
again. Never remove quarantine attributes or disable Gatekeeper. The helper's
administrator prompt is a separate approval.
`login_registration=pending` means macOS still requires Login Items approval;
reopen the app or inspect its status after granting that approval.

```text
opencodex-relayctl mode status --json
opencodex-relayctl mode request native|external|local_opencodex|relay --json
opencodex-relayctl mode apply --confirm-desktop-exited --json
opencodex-relayctl mode cancel --json
opencodex-relayctl mode recover --complete|--rollback --confirm-desktop-exited --json
opencodex-relayctl mode repair-native --expected-routing-generation N \
  --confirm-local-development-native-repair --json  # local-development only
opencodex-relayctl mode inspect-native-repair --expected-routing-generation N --json
opencodex-relayctl mode inspect-native-repair-owner --expected-routing-generation N \
  --expected-owner opencodex --installation-id ID --installation-fingerprint SHA256 \
  --native-restore-fingerprint SHA256 --ocx-executable PATH --ocx-sha256 SHA256 --json
opencodex-relayctl mode repair-native-routing --expected-routing-generation N \
  --expected-owner local_relay|opencodex --confirm-desktop-exited \
  --confirm-local-development-native-routing-repair \
  [--installation-id ID --installation-fingerprint SHA256 \
   --native-restore-fingerprint SHA256 --ocx-executable PATH --ocx-sha256 SHA256] \
  --json  # OpenCodex proof flags are required for owner=opencodex; local-development only
```

`request` records intent without terminating existing remote work. After the
selected app actually exits, `apply` waits for watcher acknowledgement, catalog
pause, and active-request drain before removing or restoring the relay-owned
`openai_base_url` and `model_catalog_json` block. It never replays or hands an
SSE, WebSocket, or tool request to another backend. A local-development-only
`repair-native` command is visible in Maintenance only for an orphaned
`recovery_required` state with both standard recovery actions unavailable.
It requires explicit confirmation, exact generation and physical path bindings,
no routing/removal journal or gate, and independent native ownership validation.
Only the routing-state file advances to `native_active`; production scope,
Codex TOML, OpenCodex, helper, and services are untouched.

If native validation is blocked only by `openai_base_url` or
`model_catalog_json`, `inspect-native-repair` returns a value-free ownership
classification: `state_only`, `local_relay`, `opencodex`, or `unavailable`.
Foreign, mixed, incomplete, and unmarked overrides remain manual-only. For the
current local Relay owner, `repair-native-routing` removes only its complete
marker block and managed interactive profile. For OpenCodex, discovery emits a
separate optional native-restore proof without changing the removal ID or
fingerprint. A manual-removal Homebrew installation can therefore be eligible
for restore while remaining ineligible for automatic package removal. The
helper re-discovers the exact Tier A/B candidate, pins the package tree into a
private snapshot, and invokes only the snapshot's bundled Bun and CLI with a
private working directory and allowlisted environment. It never executes the
selected `env node` launcher and kills the complete process group before
accepting a bounded result. After candidate selection and before Desktop exits,
`inspect-native-repair-owner` returns only `valid|invalid|unavailable` configuration state and
`enabled|disabled|unknown` Codex integration intent. Invalid or unavailable preflight never quits Desktop. Before mutation it creates a private
mode-`0600` timestamped backup without reporting its path. Native routing is
validated before generation advances. If TOML becomes native but state commit
fails, recovery remains at the old generation and the state-only repair becomes
the next explicit action. OpenCodex integration is disabled, but its Shim,
proxy, package, and data remain untouched. Only a proven no-mutation desired-state conflict
(`success=false`, config `skipped`, `changed=false`) is retried three additional times at
200/500/1000 ms. Exhaustion, invalid configuration, structured restore failure, and invalid bounded
output are reported separately as `native_owner_busy`, `native_owner_configuration_invalid`,
`native_owner_restore_failed`, and `native_owner_result_invalid`; values, paths, stdout, and stderr
never enter activity logs.

The MenuBar popover summarizes only the selected app and currently applied
route. The Control Center consumes the redacted `mode status --json` contract
and performs approved controls through the existing relayctl safety gates. Its `connection` object contains
bounded
`local_relay`, `routing_sync`, `remote_gateway`, and `catalog` values; it omits
URLs, credentials, accounts, raw health, and raw errors. `active_requests` is
`null` when the local relay cannot be reached, and Desktop backend effectiveness
remains `unverifiable` until a live Codex task succeeds.

Manual toolbar refresh is coalesced rather than dropped when polling is already
in flight. The queued manual request takes priority over cadence restart. After
schema validation it distinguishes a changed snapshot from no change, and the
completion event contains only `changed`, `generation`, and `phase`.

**Activity Log** shows up to 500 current-session events and writes the same safe
events to macOS Unified Logging category `Activity`. It provides level filtering,
search, JSON Lines copy, and a bundle-specific log command. Only allowlisted
status fields are recorded. Routing snapshots mirror the Overview and
Connection & Routing cards; sensitive paths, output, and credentials are excluded.
Clearing the in-app list leaves the system log intact.

The signed app is Dock-visible and carries a bundled `AppIcon.icns`. A Dock
reopen event and **Connection details…** both activate the app and focus its
single Control Center window; closing that window leaves the resident Relay
running. The window relies on native macOS 26 sidebar, toolbar, sheet, and
Liquid Glass controls rather than custom window chrome.
Status and explanatory text use the macOS body size and semantic label colors.
Values start beside a bounded label column; related buttons form a horizontal
group and fall back to a vertical stack only when the detail width is narrow.

The **Codex Desktop** page has a read-only configuration card for the exact
Codex TOML named by the 0600 routing binding. Every metadata check, preview, and
external-open action reloads the binding, opens the target with `O_NOFOLLOW`,
requires a regular file, and compares `lstat`/`fstat` identity before and
after access. The visible summary contains location, existence, size, modified
time, change state, Relay phase, and applied backend. While that page is
visible, metadata is checked every two seconds; each distinct replacement or
deletion produces one status refresh and one privacy-filtered event.

Complete TOML is never loaded until the user accepts the sensitive-information
warning. The selectable monospaced preview is limited to 1 MiB and valid UTF-8,
and both text and approval state are discarded when its window-owned sheet
closes. Oversized or non-UTF-8 regular files may still be opened externally
after revalidation. Default App, installed Visual Studio Code, Xcode, TextEdit,
and **Other App…** use NSWorkspace without a shell or `code` CLI and opt out of
system recent items. Relay never edits or saves the file. Activity events retain
only action target and bounded result; they exclude paths, text, hashes, and
external application output.

For **System Default**, the MenuBar app uses only the first preferred macOS
language: `ko` selects Korean, `en` selects English, and an unsupported or
unavailable language falls back to Korean. The Control Center Language menu can immediately choose System Default,
Korean, or English. The value is stored in bundle-scoped
preferences, so production and local-development apps keep independent
choices. This never changes relayctl JSON, routing phases, or CLI identifiers,
which remain stable English protocol codes.

When explicitly enabled by the macOS installer, an external gateway connection
probe runs only while `relay_active`, at most every ten minutes. It coalesces
with catalog refresh and otherwise uses one bounded, no-redirect, no-retry
`GET /v1/models` without writing the catalog. Pending native blocks new probes;
applying, native, and recovery states stop catalog/probe credential lookup and
remote egress.

### macOS External and Local OpenCodex (10100) profile boundary

Revision-3/4 macOS installs keep `external_gateway` as their canonical/default
relay configuration. The signed Control Center offers three explicit choices:
External gateway, Local OpenCodex (10100), and Native ChatGPT Codex. Local is
not a TCP-port shortcut: before it can be requested, the relay uses a bounded,
credentialless, no-proxy/no-redirect check of `/healthz` (`service:
"opencodex"`, `status: "ok"`, port `10100`) and a visible, duplicate-free
`/v1/models` response. A failed check disables Local; it never falls through to
External for the same request.

The relay owns separate External and Local catalog files and atomically selects
the matching `model_catalog_json` only after the selected Desktop app quits,
active requests drain, and `mode apply` completes. Its listener/PID stay alive;
an owner-only same-user Unix socket swaps only the immutable upstream runtime.
SSE, WebSocket, and tool work is drained, never transferred or replayed.

If an active Local runtime later loses its verified identity/catalog, it stops
the local catalog worker and returns typed `503 local_opencodex_unavailable`.
Durable state remains Local: an operator must explicitly request External or
Native, quit the registered Desktop app, and apply it.

#### OpenCodex discovery and authority boundary

**Find local OpenCodex installations…** evaluates bounded relayctl evidence in
tier order:

- **Tier A** inspects evidence directly tied to the current user, such as an
  existing enrollment, canonical absolute `PATH` launchers, and the native npm
  prefix. The MenuBar app itself does not walk arbitrary `PATH` directories.
- **Tier B** adds trusted npm/Homebrew prefixes and bounded user nvm, fnm,
  Volta, and asdf roots.
- **Tier C** is a bounded local-volume scan that requires separate approval
  after A/B find no candidate. It may be truncated and is inspection evidence,
  never mutation authority.

Discovery schema 4 adds `teardown_capability=relay_preserve_v1`,
`data_capability=preserve_only`, a bounded compatibility reason, and the
existing `homebrew_guarded_npm` / `homebrew_guard_required` state. The Swift
app reads schema 2, 3, and 4, but schema 2/3 candidates are display-only.
Automatic removal requires a user-owned, user-writable, no-elevation schema-4
Tier A/B candidate that is not Volta, has a complete unchanged
npm/Node/Bun/CLI/package execution closure, and exactly matches one reviewed
Relay teardown identity profile. The current darwin/arm64 registry admits the
exact stable releases `2.22.0`, `2.23.0`, `2.24.0`, `2.24.1`, `2.24.2`,
`2.25.0`, `2.26.0`, `2.27.0`, `2.28.0`, `2.29.0`, `2.31.0`, `2.32.0`,
`2.32.1`, and `2.33.0`. Every profile binds the official npm integrity, an
independently reconstructed complete installation-closure digest, required
module hashes, and a version-specific adapter ID. Preview releases and the
nonexistent stable `2.30.0` are not admitted. Required-module hashes remain as
bounded diagnostics; they are not the complete trust proof.

Package identity and execution are separate registries. An identity profile
binds package name, version, artifact variant, platform, registry integrity,
reviewed closure digest, adapter ID, and required-module diagnostics. The
closure digest length-delimits sorted relative paths, entry kinds, executable
bits, regular-file content digests, and raw symlink targets. UID, GID, mtime,
and non-executable permission bits stay in the separate ownership/mode and
discovery-time tree checks. The adapter registry binds an adapter ID to one
reviewed entrypoint plus its embedded source set. A future version or legitimate
same-version artifact is added as a new exact profile; execution reselects the
discovery-time artifact variant instead of accepting any profile for that
version. Discovery
does not use a version range, heuristic fallback, or partially matching
profile. No compatible profile, more than one compatible profile, a missing or
duplicate adapter implementation, or any added, removed, retargeted, or changed
closure entry is manual-only. Snapshot creation rechecks both the complete
closure digest and the independent discovery-time execution-tree fingerprint.
The UI
still requires a separate sanitized, untruncated Tier-B authority pass to
reproduce the exact installation ID and aggregate fingerprint exactly once.
Displayed package and relative paths never grant deletion authority.

`homebrew_guarded_npm` is limited to the exact arm64 global npm layout under
`/opt/homebrew`, current-user ownership, complete execution evidence, and the
ordinary Homebrew group-write bit as the only reason `exact_npm` is unsafe.
World-write, ACLs, foreign ownership, symlinks, external or conflicting
launchers, and incomplete discovery stay manual-only. Production and
local-development builds both present a reviewed `sudo` command for the generic
fixed installer, while their service IDs and CDHashes remain isolated. It
writes only the helper and LaunchDaemon under `/Library`, pins the exact app and
installer CDHashes, and verifies XPC readiness before completing. Every app
rebuild that changes a CDHash requires
the displayed `update` command. If an installer transaction remains, the app
classifies it before helper readiness, blocks safe removal, and displays only
the explicit `recover` command.

The two non-destructive handoffs remain: retain the proxy while releasing Codex
integration and Shim ownership, or retain the proxy and release integration
only. They use the exact user-approved executable and fingerprint. The legacy
alert no longer exposes `ocx uninstall`. Full removal uses a separate wizard
whose mutation selector is only an opaque 24-hex installation ID and 64-hex
aggregate fingerprint; it accepts no caller path, glob, package name, or
implicit-all selection.

The wizard remains visible during handoff and reports safety preflight, Desktop
exit, the approved OpenCodex operation, Desktop relaunch, and Relay status
verification as separate steps. Recovery, applying, unavailable, or unverified
routing is blocked before Desktop exit and before OCX invocation. After a
successful Shim handoff, status and candidates are both refreshed. Automatic
removal is authorized only after exactly one fresh candidate matches the same
canonical package root and executable with a newly bound eligible fingerprint;
missing, duplicate, or changed candidates require discovery again. A partial or
unknown helper result still triggers Desktop relaunch and status verification
so the confirmed recovery state is shown without bypassing the context-specific
recovery gate.

#### Removal contexts

Automatic removal chooses one context before discovery and never changes it
during the operation:

- **Integrated** applies only when the owner-only routing binding is ready. It
  retains the existing requirement for a healthy resident Relay and a stable,
  verified External or Native route.
- **Standalone Native** applies only when `routing-binding.json` is exactly
  absent and there are no partial Relay assets or integration recovery. It uses
  only the standard `~/.codex/config.toml`, accepts clean Native or verified
  OpenCodex-owned configuration, and verifies restored Native Codex
  configuration plus `clientIntegrations.codex=false` before any package command
  can start. It does not connect to a remote Gateway and requires no server URL,
  Gateway credentials, Relay configuration, LaunchAgent, Keychain items, or
  running Relay service. Those Relay integration assets remain unchanged before
  and after the operation.

Unsafe or invalid bindings, preview mode, integration recovery, conflicting or
damaged journals, custom `CODEX_HOME`, and local-Relay, mixed, foreign, or
unmanaged Codex configurations remain fail-closed. The automatic path is
limited to canonical app-managed roots; ambient
`XDG_CONFIG_HOME` or `OPENCODEX_HOME` overrides are rejected.

A shared owner-only lifecycle lock excludes Relay preparation, integration, and either removal
context. Recovery records bind their original context; an older integrated
record is never moved or reinterpreted as standalone removal.

The app owns the native-only relayctl operations
`discover-open-codex-native`, `inspect-open-codex-native-removal`,
`inspect-open-codex-native-data`, and `remove-open-codex-native`. Their strict
schema-1 responses bind `standalone_native`, `boundary_revision`, `native_state`,
`native_recovery_required`, and the opaque candidate identity. They do not
accept a Gateway input or caller-selected filesystem path and are not a manual
fallback for a rejected configuration.

#### Safe npm removal

Safe removal requires a selected Codex Desktop that passed the reviewed
identity policy. Integrated removal additionally requires a healthy resident
Relay and a stable verified **External or Native** route rather than active
Local OpenCodex. Standalone removal instead requires the exact Native boundary
described above and never consults remote Gateway or Relay health. Any changed
evidence requires a fresh review. Integrated removal remains `preserve_only`.
Standalone removal also preserves every OpenCodex data root by default; an
eligible `selective_trash_v1` candidate may expose a verified inventory for
explicit item selection and the existing second Trash confirmation. There is no
implicit-all selection or permanent-delete fallback.

The privileged helper can be prepared in advance from **App Information** or
**Maintenance & Recovery** in the Control Center. When approval is pending, the
card opens **Login Items & Extensions** and rechecks registration when Relay
becomes active again. For ad-hoc development, the card copies the fixed
installer command and opens Terminal without receiving the administrator
password. These assistance paths do not change Homebrew modes or remove the
OpenCodex package.

1. Integrated discovery must return schema 4 with one exact
   `relay_preserve_v1` identity profile and `preserve_only`. Standalone discovery
   must return its strict schema-1 `standalone_native` contract with a matching
   `boundary_revision`, bounded `native_state`, `native_recovery_required=false`,
   and one exact eligible candidate. Schema 2/3, unsupported versions, modified
   modules or transitive closure entries, and ambiguous registry entries remain
   visible but manual-only.
2. The review shows separately what is preserved (all OpenCodex data) and what
   will be removed (the exact npm package and verified managed integrations).
   Integrated review freezes the displayed nonzero `UInt64` routing generation;
   standalone review freezes its Native boundary fingerprint. Both accept one
   explicit package-removal confirmation. Integrated review has no data
   selector. Standalone review defaults to preserve; if the candidate advertises
   `selective_trash_v1`, selected verified inventory items require a second
   confirmation and the exact inventory revision. Neither context accepts a
   caller path/glob or implicit-all selection.
3. Immediately before execution, the app rechecks integrated route safety or
   the standalone Native boundary and, for `homebrew_guarded_npm`,
   privileged-helper readiness. Missing registration or approval blocks before
   the exact trusted Desktop is asked to quit.
4. The root helper performs `prepare` to temporarily remove Homebrew
   group-write, the app re-discovers the unchanged candidate, and the helper
   performs `commit`. It validates allowed paths with
   `openat`/`O_NOFOLLOW`/`fstat`, records original inode/device/mode in a
   root-owned mode-`0600` journal, and never deletes a package or runs npm.
5. The Go removal coordinator runs the Relay-owned adapter with verified Bun
   from a private immutable package snapshot. The adapter uses no shell,
   ambient `PATH`, caller path, or modified installed source. Its preflight is
   non-mutating. The mutating pass stops managed service/proxy state, restores
   native routing, releases managed client integration/environment/shell hooks,
   and restores a verified Shim under OpenCodex's canonical
   `codex-shim.autorestore.lock`. The Relay-owned Shim module first validates
   the complete state/wrapper/backup/destination batch and stages rollback
   hardlinks in each destination directory. It preserves every backup and the
   state file until all no-replace publications verify. A mid-batch failure is
   compensated in reverse order; an unprovable rollback retains lock, state,
   and staging evidence for explicit recovery. The ordinary lock owner record
   uses OpenCodex's schema-1 numeric timestamp contract so a dead pre-mutation
   owner remains reclaimable. A second recovery marker is created before the
   first mutation, making an interrupted or uncompensated batch deliberately
   non-reclaimable by OpenCodex until explicit recovery resolves its evidence.
6. The internal `relay_preserving_teardown` schema-1 receipt must prove the
   expected adapter ID, `data_preserved=true`, `config_root_removed=false`, and
   every required component postcondition. A refused, malformed, partial, or
   unverifiable receipt prevents npm from starting. A potentially mutating
   unknown result is not automatically retried and enters recovery.
7. Only after teardown and the context-specific integrated routing or standalone
   Native postconditions pass may standalone selective Trash move the reviewed
   items, with Native-boundary checks immediately before and after. The existing
   private npm snapshot runner then removes the package. Package absence and the
   same boundary are reverified. Finally `release` restores Homebrew modes in
   reverse order before Desktop relaunch and status refresh.

Neither exit zero nor path absence means success. A strict receipt must prove
`completed`, `package_absent`, `data_preserved`, no context recovery, final
integrated-routing or standalone-Native revalidation, and a replayable retained
terminal journal. The app first persists and reads back a schema-3
`terminal_ack_pending` recovery checkpoint containing the exact
`terminal_receipt_digest`. It then calls
`discover-open-codex-native --acknowledge-terminal-receipt-digest <digest>`.
A bare discovery or a different digest leaves the journal intact. The matching
acknowledgement revalidates the same boundary and package absence, consumes only
that exact terminal journal, and must return `ready/native`; only then does the
app clear and read back its local checkpoint before relaunching the exact
trusted Desktop. If the app exits after backend acknowledgement but before
local cleanup, the durable checkpoint retries the acknowledgement idempotently.
Relay Apply and Recover refuse any retained or malformed standalone journal, so an
interrupted acknowledgement cannot create split-brain integration state. A pre-commit
Homebrew guard crash is restored automatically; an ambiguous post-commit state
stays protected and requires explicit recovery. Production and development
share one system lock. The official OpenCodex package and its data are never
patched by this flow, and the separate `opencodex/` checkout is not used.

#### Interrupted recovery

- Teardown and package children each write a typed active-execution witness
  (kind, attempt, and boot session) and fsync it before launch. While a witness
  is present, routing changes, replay, package resume, and finalization are all
  denied.
- A routing refusal before child start and a malformed receipt after verified
  child cleanup use a durable resolution protocol: fsync the finite resolution
  marker, durably park routing when required, then clear the active witness into
  an operation retry, package retry, or data-refresh phase. A crash at either
  boundary resumes from that marker. `routing_recovery_persisted` is emitted
  only after both the routing park and journal phase transition succeed.
- A legacy integrated data-inventory/Trash journal is not converted to schema 4
  or reinterpreted as standalone removal. It remains fail-closed and requires
  reviewed manual recovery. New standalone inventory and Trash witnesses stay
  bound to their schema-6 `standalone_native` journal and Native boundary.
- A validated failure before cleanup intent is not durable removal recovery.
  Only the exact allowlisted request, candidate, data-policy, or teardown-
  preflight sequence—with no cleanup journal, child-start, package/data/routing
  mutation, reboot, or unknown-process evidence—clears the speculative Swift
  `.inFlight` checkpoint. The exact trusted Desktop is relaunched, status is
  refreshed, and the original bounded failure code remains in the wizard.
  Every post-intent or ambiguous receipt remains fail-closed in recovery.
- `process_cleanup_unverified` requires a **whole-Mac reboot**. A PID check,
  waiting, restarting MenuBar/Relay, or relaunching Codex is not proof. The
  helper must attest a changed platform boot session and then verify exact
  package/launcher absence.
- Changed-boot reconciliation is kind-specific: teardown clears its active
  witness back to the existing `operation_intent` and parks routing without
  replaying teardown; package uses the existing exact-absence or
  residual-pending branch. A fresh routing recovery and review are required
  before any later lifecycle action.
- Watcher/controller admission stays fail-closed while package execution is
  ambiguous or `routing_recovery_required` is unresolved. Saved reboot/in-flight
  recovery may use only its narrow recovery predicate and the validated durable
  generation, including the exact unreachable/unreachable health projection;
  ordinary uninstall remains denied. A gated projection with
  `generation=0` can render its opaque fail-closed state but cannot authorize
  recovery, removal, or routing mutation. Saved journal-backed routing recovery
  must match its stored generation. Unlike reboot/in-flight reconciliation, the
  routing action itself requires a healthy relay and an actionable Complete
  capability. The helper binds the saved opaque installation ID/fingerprint,
  reviewed generation, and validated routing-transaction witness before the
  controller mutates routing. Its exact journal gate remains durable while the
  controller recovers and is cleared only after a new stable state,
  acknowledged live relay health, and exact Codex ownership are reverified. A
  failed or crashed recovery therefore remains retryable. Observing a strictly
  newer safe generation only checkpoints it with the same opaque selector and
  invalidates the current review; a second explicit action is required before
  recovery or phase continuation. Do not edit/delete the journal or issue
  another lifecycle mutation; resume the saved wizard, recover routing, and
  review the fresh generation.

User-visible receipts and versioned recovery sessions contain only opaque IDs,
mode, adapter ID, component states, and finite codes—not absolute paths,
module hashes, child stdout/stderr, raw errors, credentials, data, or live logs.
This does not mean the helper's owner-only durable cleanup journal is path-free.
Reboot recovery still requires separate disposable-installation acceptance.

## Install on a general Linux client

Enter credentials with an editor rather than command-line values:

```bash
install -d -m 0700 ~/.config/opencodex-relay
umask 077
${EDITOR:-vi} ~/.config/opencodex-relay/credentials.env
chmod 0600 ~/.config/opencodex-relay/credentials.env
```

Run the same signed installer; it selects `linux/amd64` or `linux/arm64` from
`uname`. Verify with:

```bash
systemctl --user status opencodex-relay.service
journalctl --user -u opencodex-relay.service --since today --no-pager
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
```

Use `sudo loginctl enable-linger ubuntu` only on a Remote host where the user
service must survive SSH logout and after operational approval.

## Linux Remote Control home

Remote hosts use `/home/ubuntu/.codex-remote-opencodex` instead of ordinary
`~/.codex`. Install the reviewed Remote manager/wrapper/timer first. On Ubuntu
24.04, validate the narrow bubblewrap AppArmor path without disabling the
global user-namespace restriction:

```bash
sudo ./pilot/scripts/configure-codex-linux-sandbox.sh --user ubuntu
```

The legacy managed Remote baseline is bare `gpt-5.6-luna`. In local-relay,
the manager accepts either a case-insensitive exact bounded-policy root such as
`opencode-go-responses/gpt-5.6-luna`, or a root that appears byte-exactly once
in the materialized catalog. It reports the former as `bounded_json` and the
latter as `passthrough`, preserving either selection. Removing the Cursor
compatibility adapter changed an
earlier 40-entry snapshot to a historical 26-entry snapshot. The Relay `0.2.1`
acceptance observed 27 entries on both Remotes. Catalog size is not a fixed
contract: compare the current upstream-visible set with the current reader
result. These are central `Model_Catalog` changes, not Remote-local filters.
An invalid policy/catalog or missing, duplicate, malformed, or unlisted root
fails before config writes or daemon restart. Apply and verify only the
dedicated Remote config; `status` also emits
`default_model_relay_mode=bounded_json|passthrough`:

```bash
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh \
  set-default-model --allow-remote-interruption
~/.local/lib/opencodex-relay/manage-remote-codex-home.sh verify-default-model
```

Classify the Remote before installing a relay. `"mode": "external"` is mandatory for
the external enrollment path. A `"mode": "loopback"` Remote must not use that path or
edge credentials. The deployed central x86 Remote instead uses `local-relay`,
a loopback relay service with `local_opencodex` that injects no edge credential
and preserves normal Native Codex authorization. Both the bare catalog-visible
model and the qualified bounded-policy model are valid selections. Bare
passthrough skips Relay normalization only; it still follows Relay to loopback
OpenCodex and does not bypass the relay to an upstream service.

```bash
jq -e '.mode == "external"' ~/.config/opencodex-relay/remote-opencodex.json >/dev/null || {
  printf '%s\n' 'Refusing external relay enrollment on a loopback Remote.' >&2
  exit 2
}
```

`install-remote-codex-relay.sh install` repeats this external fail-closed check
before fetching an artifact. Its explicit `install-local` action owns the
loopback case. Architecture (`linux/amd64` or `linux/arm64`) selects a binary;
it never decides the topology.

Install the relay against the dedicated catalog and leave daemon restarts to
the Remote manager:

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --config /home/ubuntu/.config/opencodex-relay/relay.json \
  --codex-config /home/ubuntu/.codex-remote-opencodex/config.toml \
  --catalog-path /home/ubuntu/.codex-remote-opencodex/opencodex-catalog.json \
  --codex-executable /home/ubuntu/.codex-remote-opencodex/packages/standalone/current/codex \
  --manage-app-server false
```

For a public GitHub Release, refresh the Remote-home automation once and then
use the wrapper below. It installs the relay installer/service bootstrap but
never copies a GitHub token, public PEM, `credentials.env`, or `auth.json`.

```bash
cd /path/to/OpenCodex-OCI-Gateway/pilot/scripts
./install-remote-codex-home.sh install --with-relay-bootstrap

~/.local/lib/opencodex-relay/install-remote-codex-relay.sh install 1.2.3 \
  --github-repo OWNER/opencodex-relay-releases \
  --public-key ~/.config/opencodex-relay/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --allow-remote-interruption
```

Add `--migrate-legacy` when the target is `pw_opencodex`, `opencodex`,
`pw_opencodex_remote`, or the exact old loopback URL at `127.0.0.1:10100` or
`localhost:10100`. The wrapper never guesses or removes another custom provider
or a user-owned root `openai_base_url`; inspect its configuration and backup first.

Provider migration must run in the relay installer **before** it enables native
routing. Therefore add `--migrate-legacy` to the raw installer command above,
not to the routing command that only records the mode and restarts the daemon.
Perform that potentially interrupting work only in a maintenance window:

```bash
~/.local/lib/opencodex-relay/configure-remote-codex-routing.sh \
  enable-relay --allow-remote-interruption

~/.local/lib/opencodex-relay/configure-remote-codex-routing.sh status
~/.local/lib/opencodex-relay/update-remote-codex.sh status
codex debug models | jq '.models | length'
```

Schema v1 requires `routing_mode` and rejects a missing field. The migration
script writes `"routing_mode": "relay"` only after native configuration is ready,
then restarts the managed daemon. In relay mode the wrapper unsets all three
admission variables before executing native Codex.

## Configuration reference

An external normalizer configuration contains no credential value:

```json
{
  "listen_address": "127.0.0.1:18180",
  "upstream_mode": "external_gateway",
  "upstream_base_url": "https://REPLACE_WITH_API_HOSTNAME/v1",
  "voice_enabled": false,
  "connection_probe": { "enabled": true },
  "credentials": {
    "source": "keychain",
    "file": "/path/to/user/.config/opencodex-relay/credentials.env"
  },
  "responses": {
    "websocket_mode": "http_fallback",
    "model_modes": {
      "opencode-go-responses/gpt-5.6-luna": "bounded_json"
    },
    "scheduler": {
      "interactive_listen_address": "127.0.0.1:18182",
      "max_classifications": 8,
      "max_pending_requests": 24,
      "max_pending_encoded_bytes": 536870912,
      "queue_timeout_ms": 60000,
      "max_general_upstream": 4,
      "interactive_reserved_upstream": 1,
      "max_concurrent_transforms": 2,
      "max_open_deliveries": 16
    }
  },
  "catalog": {
    "owner": "relay",
    "path": "/path/to/user/.codex/opencodex-relay-catalog.json",
    "refresh_interval": "10m",
    "manage_app_server": false,
    "codex_executable": "codex"
  }
}
```

`listen_address` accepts only `127.0.0.1` or `::1`. `external_gateway` requires
an absolute HTTPS `/v1` URL, keychain/file credentials, and
`catalog.owner=relay`. `local_opencodex` instead accepts only
`http://127.0.0.1:10100/v1` or `http://[::1]:10100/v1`, requires
`credentials.source=none` and `catalog.owner=remote_manager`, disables the
environment proxy, and never injects edge credentials. Missing new fields keep
the old external/passthrough/relay defaults. Refresh interval is at least one
minute. Automatic AppServer restart defaults to `false`; Remote homes must use
`false`. `connection_probe.enabled` is a macOS-installer opt-in for the bounded
ten-minute external-gateway observation; it is invalid for `local_opencodex`
and is always inactive while routing is native or parked.

When any `responses.model_modes` entry is configured,
`responses.websocket_mode` must be `http_fallback`. Model keys match only
case-insensitive exact strings; whitespace, case-fold duplicates, colon-family
inheritance, and provider-alias guessing are rejected. The bounded path supports
identity/zstd requests, modifies only top-level `stream:true`, performs one
upstream request, and synthesizes the validated terminal JSON as canonical
HTTP/SSE. It does not change the built-in `opencode-go` Chat Completions mapping.
Scheduler zero/absent values load the defaults shown above; explicit values
must remain within the relay validator's bounded ranges.

A colocated Remote uses this form:

```json
{
  "upstream_mode": "local_opencodex",
  "upstream_base_url": "http://127.0.0.1:10100/v1",
  "credentials": { "source": "none" },
  "responses": {
    "websocket_mode": "http_fallback",
    "model_modes": {
      "opencode-go-responses/gpt-5.6-luna": "bounded_json"
    }
  },
  "catalog": { "owner": "remote_manager" }
}
```

The abbreviated object above shows the mode-specific fields; retain the normal
listen, catalog path/interval, Codex executable, and AppServer-management fields
in the actual configuration. Before enabling a local normalizer, the Remote
routing helper validates the effective colocated OpenCodex config through the
service account and requires `images.videoBridgeEnabled=false` or absent. It
derives the exact config source by running the root-owned Runtime Adapter's
`describe --json` through passwordless `sudo`, then runs adapter-owned
`ocx config validate/show --json` as
the `opencodex` account with `OPENCODEX_HOME` and `CODEX_HOME` unset. The adapter
contract selects the canonical Node, entry, and service HOME; the routing helper
does not duplicate those paths. It streams redacted config to `jq` without
writing it to disk and fails closed before routing mutation if the adapter,
contract, description, source, or service-account check is unavailable because
the relay cannot infer that server-side feature from a request.

### Dual listeners and the explicit interactive profile

Every installed relay has a general listener and a distinct numeric-loopback
interactive listener in the same process. The general address remains
`listen_address` (normally `127.0.0.1:18180`). The interactive address is
`responses.scheduler.interactive_listen_address`; when omitted, it defaults to
port `18182` in the general listener's address family. Another numeric-loopback
port is allowed when the relay validator accepts it and it differs from the
general listener.

The general Codex configuration remains authoritative for ordinary TUI,
`exec`, `review`, AppServer, and daemon work. The installer also atomically
maintains `$CODEX_HOME/opencodex-relay-interactive.config.toml` for an explicitly
selected side session:

```toml
# opencodex-relay-managed-interactive-profile-v1
openai_base_url = "http://127.0.0.1:18182/v1"
model_catalog_json = "/ABSOLUTE/CODEX_HOME/opencodex-relay-catalog.json"
```

The marker is ownership metadata, not another setting. The profile sets no
model, reasoning option, agent limit, or hidden fallback. If a same-name file
exists without the exact marker, installation and Remote activation fail before
overwriting it. Selection is never automatic:

```bash
codex --profile opencodex-relay-interactive
codex exec --profile opencodex-relay-interactive 'REPLACE_WITH_PROMPT'
```

Before a service swap, the installer permits an occupied interactive port only
when the currently active managed relay answers both reviewed health contracts.
Otherwise the configured port must be free. No process is killed; a bind race
causes activation failure and transaction rollback.

On Linux only, an explicit automatic-restart opt-in requires both values below.
`CODEX_HOME` in the target AppServer environment must exactly equal
`app_server_home`; a command-line match alone is never sufficient. macOS has no
equivalent verified identity source in this implementation, so it leaves the
marker pending and requires a manual restart.

```json
"manage_app_server": true,
"app_server_home": "/home/REPLACE_WITH_USER/.codex"
```

`relayctl init --force` replaces the complete relay JSON. It is not a routine
update command; back up and inspect the current configuration first.

## Catalog and AppServer lifecycle

At startup and on each refresh interval, the relay:

1. reads explicit semver from the selected Codex binary;
2. fetches authenticated `/v1/models?client_version=...` with an 8 MiB limit;
3. accepts `.models` or `.data` and removes `visibility: "hide"` entries;
4. rejects empty catalogs, missing IDs, duplicate visible IDs, and multiple JSON values;
5. atomically replaces the `0600` catalog only when changed, retaining
   `.previous` and `.restart-pending`;
6. only after the explicit Linux opt-in, takes a nonblocking quiescence gate
   and signals AppServer processes whose environment proves the exact configured
   `CODEX_HOME`, while active requests are absent and new request admission is
   excluded. If identity is absent/different/unavailable or the gate is busy,
   it signals none of the candidate processes and the marker remains for the
   next activation tick.

For an external Remote home, `manage_app_server=false` is intentional. The relay is the
sole catalog writer; `opencodex-remote-relay-catalog-activation.timer` checks
the relay's `.restart-pending` marker every minute and calls the Remote manager
after observing an `active_requests == 0` health snapshot. Unlike resident
activation, this cross-process check does not hold the relay admission gate;
strict no-new-request activation therefore requires a maintenance window. The
older ten-minute manager timer keeps
the legacy/loopback fetch behavior and is disabled on a relay Remote. Even if
`refresh --restart` is invoked manually in relay mode, it only reports
`relay_catalog_refresh=owned_by_relay`; the dedicated activation timer remains
the sole marker activator. Do not add another catalog normalizer or activator
to the same Remote home.

For `"routing_mode": "local-relay"`, the relay's `catalog.owner=remote_manager`
disables its catalog lifecycle completely. The existing Remote manager keeps
fetching the colocated `10100` catalog and remains the only writer and marker
activator; only Native Responses data traffic moves to `18180`.

For resident local activation, “idle” means **the quiescence gate excluded both
in-flight and newly admitted relay requests through the restart operation**. It
does not prove that no Desktop window or logical user session is open. Automatic
restart is off unless the Linux home identity opt-in above is configured. The
relay never restarts an open CLI TUI; on macOS it always leaves catalog
activation for an explicit CLI/Desktop restart.

The Remote manager recognizes both legacy and relay marker names. Its timer can
restart the managed daemon after a verified change or pending marker. It avoids
restarting a request visible in the sampled health state, but a request admitted
immediately afterward can still be disconnected; use an explicit maintenance
window when that residual cross-process race is unacceptable.

## Voice double opt-in

Voice/Realtime is closed at both boundaries by default:

1. set local `relay.json` `voice_enabled` to `true`;
2. restart the local user service;
3. enable the central feature gate.

```bash
# macOS
launchctl kickstart -k "gui/$(id -u)/io.github.novelkr.opencodex-relay"

# Linux
systemctl --user restart opencodex-relay.service

# Central gateway
sudo ./pilot/scripts/configure-gateway-features.sh voice on
sudo ./pilot/scripts/smoke-test.sh
```

Either local `false` or central `off` blocks Voice. For emergency closure,
turn the central gate off first, then restore local `false`. HTTP setup or a
WebSocket `101` does not prove the audio/WebRTC media path; record a real
connect, bidirectional-audio, termination, and reconnect test.

## Operations and diagnosis

```bash
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
curl --fail --silent http://127.0.0.1:18180/__relay/healthz
curl --fail --silent http://127.0.0.1:18182/__relay/healthz
```

The general endpoint must report `listener_lane=general`; the interactive
endpoint must report `listener_lane=interactive`. Both report the general and
interactive addresses, configured scheduler limits, shared `active_requests`,
and nonnegative classification, queue, upstream, transform, delivery, and
rejection counters. They expose no credential value.

| Symptom | First check | Meaning/action |
| --- | --- | --- |
| `relay_running=0` | launchd/systemd and relay error log | Service stopped, invalid config, or port collision |
| `credential_unavailable` | Keychain names/account or Linux owner/mode | At least one credential is absent or unsafe |
| Central `401` | Service Token policy and gateway-key rotation | Separate Cloudflare admission from Nginx rejection |
| Only default models | Resident catalog lifecycle/logs, catalog path, and `codex debug models` | Stale catalog or another Codex home; `relayctl catalog refresh` is reporting-only in revision 4 |
| Catalog `.restart-pending` marker remains | Changed catalog with the resident lifecycle, or an active/unidentified AppServer | Restart the same-home CLI/Desktop manually, or use the explicit Linux identity opt-in; do not broadly kill processes |
| Desktop picker unchanged | Full restart with the same Codex home | Picker can be startup-loaded |
| Remote UI offline | Daemon status, proxy handshake, registration state | SSH and Remote Control are separate lifecycles |
| Dedicated config and selected model/reasoning differ | `home_project_trust` and ordinary `~/.codex/config.toml` | Check for a trusted SSH-home project overlay, then use `isolate-home-project-config` |
| Model-picker `base URL is overridden` warning | `openai_base_url`, authenticated catalog, real Responses call | Expected for proxy/base-URL routing; do not bypass OpenCodex merely to silence it |
| Legacy loopback `responses_websocket` `426` log | The following HTTP/SSE Responses result | A successful HTTP fallback is not evidence of a model or Remote Control failure; verify native Responses WebSocket separately after relay deployment |
| Model is listed but its request is rejected | Real Responses error for that model and account selection | Keep models unless `visibility: "hide"`; select an account that supports the model |
| Voice `404` | Local JSON and central feature flag | One or both gates are off |
| bubblewrap warning | Narrow AppArmor setup and `bwrap --unshare-user` | Keep the global restriction enabled |

macOS logs are under `~/Library/Logs/opencodex-relay/`; Linux uses
`journalctl --user -u opencodex-relay.service`. Relay logs contain method,
path, and error type, not credential values or response bodies.

## Update and rollback

Re-run the installer with a newer signed version and the same upstream. The
relay JSON and catalog are retained. See [`updates.md`](updates.md) for the
combined Codex/OpenCodex release order.

Local uninstall first records a native request. If a relay route is active, it
keeps the service installed until the selected Desktop has exited and the
operator reruns it with `--confirm-desktop-exited` (or completes the switch in
the MenuBar app). Only after native apply succeeds does it remove the service,
managed login item, relay-owned block, and marker-owned interactive profile;
configuration, catalog, and version directories remain:

```bash
./client/relay/scripts/install-relay.sh uninstall \
  --codex-config ~/.codex/config.toml \
  --confirm-desktop-exited
```

If migration was used, inspect its timestamped backup before restoring it.
Automatic rollback never guesses an arbitrary custom provider.

Remote-home rollback is not fully automated. In a maintenance window, identify
the exact migration backup, restore the Remote `config.toml`, change
`"routing_mode": "legacy"` in the owner-only `remote-opencodex.json`, restart the
managed daemon, uninstall the relay against the Remote config, then revalidate
models, daemon state, and Remote Control. If backup identity or legacy
credentials are uncertain, repair the relay rather than attempting rollback.

## Validation and completion criteria

Repository checks:

```bash
bash -n pilot/scripts/*.sh ops/oci/*.sh client/relay/scripts/*.sh
python3 -m unittest discover -s pilot/tests -p 'test_*.py'
(cd client/relay && go test -count=1 ./... && go vet ./...)
(cd client/relay && go test -race -count=1 ./internal/handoff ./internal/routing)
(cd client/relay/macos/OpenCodexRelay && swift test)
git diff --check
```

### Codex AppShot workspace acceptance

AppShot validation is task-scoped because Codex creates the writable-root
profile when a workspace task is opened. A task retaining a writable alias
whose parent is a symbolic link can fail before any command or screenshot tool
runs, even when that alias resolves to the same inode as the trusted physical
checkout. Do not delete the filesystem symlink as remediation.

On this macOS checkout, keep the project trust entry on
`/path/to/OpenCodex-OCI-Gateway`, remove only the
stale workspace alias from Codex, reopen that physical path, restart Codex, and
create a short new validation task. Record these independent canaries:

1. a sandbox command runs without a symlink writable-root error;
2. the Relay Control Center is discovered as a shareable window;
3. a new AppShot is attached and rendered as an image in the task.

The old task remains useful evidence but cannot prove the refreshed permission
profile. If step 1 passes and step 3 fails, record the bounded capture error and
verify Codex Screen Recording and Accessibility grants; do not change Relay
window-sharing code when its window already reports sharing enabled.

`opencodex/` is a separate upstream checkout that the outer clone does not
vendor, publish, or pin. Local source validation for this change requires both
the [`d9de89557c3bd154e5f1508125def7c8789ac8c5`](https://github.com/lidge-jun/opencodex/commit/d9de89557c3bd154e5f1508125def7c8789ac8c5)
baseline from `https://github.com/lidge-jun/opencodex.git` and the separately
reviewed nested working-tree diff. Do not assume a clean outer clone contains
that directory or change set, and deploy a reviewed published package version
rather than this source tree.

```bash
git -C opencodex rev-parse HEAD
(cd opencodex && bun run typecheck && bun run test && bun run privacy:scan)
git -C opencodex diff --check
```

Static checks do not prove live availability. Deployment completion requires:

1. central `nginx -t`, host smoke, and external Cloudflare/SSE smoke;
2. each client relay health, signed installed version, and credential lookup;
3. visible catalog count matching `codex debug models`;
4. a real CLI Responses/tool call;
5. a Desktop project/session and picker check using the same Codex home;
6. daemon running, local proxy WebSocket `101`, and Remote UI online on each
   `"mode": "external"` Linux Remote host;
7. separate live Images or Voice acceptance when either feature is enabled.

In addition to the static checks in [`testing.md`](testing.md), record separate
macOS discovery/removal acceptance against a disposable installation without
live credentials or user data:

1. empty/malformed Codex bundle/Team metadata and signature changes block
   discovery, persistence, quit/relaunch, routing, and removal;
2. only schema-4 Tier A/B candidates matching exactly one reviewed identity
   profile and adapter receive automatic removal authority; schema 2/3, Tier C,
   Volta, root/elevation, modified, and manual candidates remain manual-only;
3. a successful fixture preserves every OpenCodex data-root byte while removing
   only the verified package and managed integrations, and npm cannot start
   before the strict teardown receipt passes;
4. generation changes, unknown receipt keys, stage mismatches, and missing
   package/routing/relay terminal proof block success and relaunch; and
5. injected teardown/package interruption requires bounded recovery, a real Mac
   reboot satisfies boot-session-attested process recovery, and routing recovery
   keeps admission closed.

The independently reviewed Codex identity is now shipped in the production
plist, but implementation and static-test completion are not production
readiness until the acceptance above is performed and recorded.

Prior session `019fd7cc-0f03-76c3-9da1-4d36a7bf85a7` supplied the x86/ARM
Linux Remote-host and bubblewrap/AppArmor constraints used by this design. Its
screenshots are historical input, not proof of current server health or live
deployment of this implementation.

## Official Codex sources

These public documents were checked on 2026-08-07. The relay and central
gateway are repository-specific; the official sources establish only the
Codex configuration, process, and product-surface boundaries.

- [Codex advanced configuration](https://learn.chatgpt.com/docs/config-file/config-advanced)
- [Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
- [Codex CLI command reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli)
- [Codex Remote connections](https://learn.chatgpt.com/docs/remote-connections)
- [Image generation](https://learn.chatgpt.com/docs/image-generation)
- [ChatGPT Voice](https://learn.chatgpt.com/docs/features/voice)

## Implementation map

| Responsibility | Path |
| --- | --- |
| Relay daemon | [`../client/relay/cmd/opencodex-relay/`](../client/relay/cmd/opencodex-relay/) |
| Control/migration CLI | [`../client/relay/cmd/opencodex-relayctl/`](../client/relay/cmd/opencodex-relayctl/) |
| OpenCodex tier discovery/execution closure | [`../client/relay/internal/handoff/npm_discovery.go`](../client/relay/internal/handoff/npm_discovery.go), [`execution_closure.go`](../client/relay/internal/handoff/execution_closure.go) |
| Removal coordinator/runner/journal | [`../client/relay/internal/handoff/removal.go`](../client/relay/internal/handoff/removal.go), [`npm_runner.go`](../client/relay/internal/handoff/npm_runner.go), [`removal_journal.go`](../client/relay/internal/handoff/removal_journal.go) |
| Codex Desktop signature trust | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/CodexDesktopTrust.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/CodexDesktopTrust.swift) |
| Swift removal protocol/receipt verifier | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayCore/OpenCodexRemoval.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayCore/OpenCodexRemoval.swift) |
| Removal flow/wizard | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalFlow.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalFlow.swift), [`OpenCodexRemovalWizardView.swift`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelay/OpenCodexRemovalWizardView.swift) |
| Korean-first localization | [`../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayLocalization/`](../client/relay/macos/OpenCodexRelay/Sources/OpenCodexRelayLocalization/) |
| Relay-owned package identity/teardown adapter | [`../client/relay/internal/handoff/teardown_compatibility.go`](../client/relay/internal/handoff/teardown_compatibility.go), [`../client/relay/internal/handoff/adapter/relay_preserve_v1.ts`](../client/relay/internal/handoff/adapter/relay_preserve_v1.ts) |
| Exact route contract | [`../client/relay/internal/compat/routes.go`](../client/relay/internal/compat/routes.go) |
| Credential resolver | [`../client/relay/internal/credentials/`](../client/relay/internal/credentials/) |
| Catalog lifecycle | [`../client/relay/internal/catalog/`](../client/relay/internal/catalog/) |
| Codex config marker/migration | [`../client/relay/internal/codexconfig/`](../client/relay/internal/codexconfig/) |
| Signed release | [`../client/relay/internal/release/`](../client/relay/internal/release/) |
| Platform installer | [`../client/relay/scripts/install-relay.sh`](../client/relay/scripts/install-relay.sh) |
| Central allowlist | [`../pilot/nginx/opencodex-api.conf`](../pilot/nginx/opencodex-api.conf) |
| Central Voice gate | [`../pilot/scripts/configure-gateway-features.sh`](../pilot/scripts/configure-gateway-features.sh) |
| Remote-mode migration | [`../pilot/scripts/configure-remote-codex-routing.sh`](../pilot/scripts/configure-remote-codex-routing.sh) |
| Remote catalog/daemon manager | [`../pilot/scripts/manage-remote-codex-home.sh`](../pilot/scripts/manage-remote-codex-home.sh) |
