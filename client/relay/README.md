# OpenCodex native compatibility relay

The canonical operations and acceptance guide is
[`../../docs/local-codex-relay.md`](../../docs/local-codex-relay.md). 한국어 구성요소
안내는 [`README.ko.md`](README.ko.md), 한국어 정본은
[`../../docs/local-codex-relay.ko.md`](../../docs/local-codex-relay.ko.md)를 참조하세요.

This directory builds the client-side compatibility layer for both a colocated
OpenCodex and an externally hosted gateway. It keeps native Codex CLI, the
local Codex AppServer, and Codex Desktop on the built-in `openai` provider.
Topology is selected explicitly at startup; it is never inferred from CPU or
reachability and a failed request is never replayed through the other route.

```text
local_opencodex:  Native Codex -> 127.0.0.1:18180 relay -> 127.0.0.1:10100 OpenCodex
external_gateway: Native Codex -> 127.0.0.1:18180 relay -> Cloudflare/Nginx -> OpenCodex
```

The relay writes a small marker-owned block in the selected `config.toml`:
`openai_base_url` and `model_catalog_json`. It does not replace native OAuth,
the built-in `openai` provider, local project settings, or Desktop-native UI
features. It refuses to overwrite a user-owned base URL or a custom root
provider.

Desktop uses this path only when its local Codex AppServer reads that same
selected Codex home/configuration. Calls made by a separate Desktop-native
product control plane are deliberately not intercepted.

## Prebuilt macOS app: connect a self-hosted server

For a downloaded `OpenCodexRelay.app.zip`, the normal user path does not require
the source repository, a package version, an output directory, or a manifest
signing key:

1. Verify the download source and published SHA-256, expand the archive, and
   open the actual app. If it is outside an Applications folder, Settings makes
   **Move to Applications** the only enabled onboarding action. It first tries
   `/Applications/OpenCodexRelay.app`; after a write-permission failure it asks
   before using `~/Applications/OpenCodexRelay.app`. The source remains in place
   until the verified destination instance starts. The destination verifies the
   exact source process has exited and safely clears any replaced-app backup
   before self-hosted settings are enabled. The user then independently chooses
   to keep the original or move the unchanged original to the Trash.
2. Open the app from its new location in Finder. If macOS blocks it, use **System Settings → Privacy
   & Security → Open Anyway**, then confirm **Open**. Do not remove quarantine
   attributes or disable Gatekeeper.
3. Open **Control Center → Settings → Self-hosted server connection** and enter
   only the server address, authentication method, and credentials required by
   that method. HTTPS accepts an origin or `/v1`; private-LAN HTTP is limited to
   an RFC1918 IPv4 or IPv6 ULA literal and requires an explicit plaintext
   acknowledgement for every supported authentication profile because native
   Codex Authorization and request content are not protected by TLS.
4. Choose **Prepare Relay**, then **Test connection**. Preparation creates only
   current-user configuration, runtime, LaunchAgent, and routing-binding files;
   it does not open Terminal, run `sudo`, write `/Library`, or edit Codex
   `config.toml`.
5. After a successful connection test, choose **Switch Codex to this server**.
   A recognized legacy OpenCodex route is backed up at mode `0600` and migrated
   inside the switch transaction. Unknown providers or user overrides remain
   untouched and must be reviewed externally.
6. OpenCodex package removal is optional and is offered only after the switch.
   Data removal means moving revalidated, explicitly selected items to the
   macOS Trash; permanent deletion is not available.

The three authentication profiles are **None**, **Gateway API key**, and
**Cloudflare Access + Gateway API key**. Existing Keychain values are shown only
as configured/missing and are never revealed, copied, or logged.

The menu bar icon keeps its compact status popover on left click. Right click
offers only **Open Control Center…**, **Refresh**, **Login Item Settings…**, and
**Quit**; routing, recovery, and removal remain deliberate Control Center flows.

## Supported targets and credential storage

The release builder emits:

- one self-contained, ad-hoc-signed `OpenCodexRelay.app.zip` with Hardened
  Runtime for the supported
  `darwin/arm64` (Apple Silicon macOS 26+) path; and
- `linux/amd64`;
- `linux/arm64`.

In `external_gateway` mode on macOS, the relay resolves only the credentials
required by the selected authentication profile from the login Keychain. It
never places them in `config.toml` or the launchd plist. The following commands
are manual diagnostic fallbacks, not steps every user must perform:

```bash
security add-generic-password -U -a "$USER" -s opencodex-relay-cf-access-client-id -w
security add-generic-password -U -a "$USER" -s opencodex-relay-cf-access-client-secret -w
security add-generic-password -U -a "$USER" -s opencodex-relay-gateway-api-key -w
```

On macOS, the preferred interactive path is **Control Center → Settings →
Connect a self-hosted server**. The onboarding prepares the current-user Relay
integration, edits the address, selects an authentication profile, tests the
connection, and adds or replaces only the required Keychain values. Existing
values are never revealed, copied, or logged; unused values are retained but
ignored. New items use a login-Keychain ACL limited to the Desktop app and
`/usr/bin/security`. Replacement preserves existing ACL entries while repairing
either required trust entry, and the save is reported successful only after
non-interactive readback works.

**Test connection** performs exactly one authenticated
`GET /v1/models?client_version=…` with a five-second deadline, no redirect or
retry, the shared catalog parser, and an 8 MiB response cap. It does not change
the catalog or configuration. **Apply** always repeats that test. If External
is active, Relay atomically saves the config, parks admission, drains existing
requests, and hot-swaps the immutable External runtime without quitting Codex
Desktop. Native or Local mode saves the address for the next External switch.
Credential saves remain durable if validation fails; the UI then distinguishes
validation required, credential-combination mismatch, unreachable, and invalid
catalog response. The success receipt contains only the config digest, Keychain
modification times, verification time, and bounded result code.

The same owner-bound interface is available for diagnostics. The candidate
address is JSON stdin, never an argument:

```bash
opencodex-relayctl gateway inspect --json
opencodex-relayctl gateway test --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
opencodex-relayctl gateway apply   --expected-config-digest REPLACE_WITH_INSPECTED_SHA256   --expected-routing-generation REPLACE_WITH_INSPECTED_GENERATION   --json <<'JSON'
{"upstream_base_url":"https://REPLACE_WITH_API_HOSTNAME/v1"}
JSON
```

`apply` refuses stale config digests, routing generations, credential changes,
pending transitions, and recovery state. A failed live swap restores the prior
config and runtime; if that rollback cannot be proven, routing stays
`recovery_required`.

In `external_gateway` mode on Linux, use an owner-only `~/.config/opencodex-relay/credentials.env` containing
only literal `NAME=value` rows for these three names:

```text
CF_ACCESS_CLIENT_ID=REPLACE_WITH_SERVICE_TOKEN_ID
CF_ACCESS_CLIENT_SECRET=REPLACE_WITH_SERVICE_TOKEN_SECRET
OPENCODEX_GATEWAY_API_KEY=REPLACE_WITH_GATEWAY_KEY
```

Set the directory to `0700` and the file to `0600`. Do not use `export`, shell
substitution, or any extra variable in this file; the relay treats it as data,
not shell code.

`local_opencodex` instead requires `credentials.source=none`, accepts only the
fixed numeric loopback OpenCodex URL, strips caller-supplied outer admission
headers, preserves native `Authorization`, and disables environment proxies.

## Install and migrate a local client

The release signing key is kept outside the repository. Obtain its trusted
Ed25519 public PEM through an independent reviewed channel, then install an
explicit version:

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1
```

The installer verifies the signed manifest and the SHA-256 of both the relay
and control binary before changing its `current` symlink. If launchd or
systemd activation fails, it transactionally restores the prior selected
target, relay JSON, selected Codex configuration/routing, service artifact,
and manager active/enabled state before returning the failure. On a first
enrollment, that means removing the newly created routing block, relay JSON,
interactive profile, and plist/unit rather than leaving Codex pointed at an
unavailable loopback relay. An existing same-name interactive profile without
the exact opencodex-relay marker fails closed and is never overwritten. A release
target created by the failed invocation is also removed after
a complete rollback; a pre-existing release is never removed, and a target is
retained if it may still be selected after an incomplete rollback. The installer
creates `~/.config/opencodex-relay/relay.json` only when absent,
preserves that non-secret configuration on later installs, writes no credential
to a service definition, and starts a user launchd/systemd service.

On macOS, install and uninstall hold an owner-only source lifecycle reservation
from the first persistent mutation through activation, verified rollback, or
complete teardown. The LaunchAgent helper's mutating commands are internal to
those reserved transactions; invoking them directly is rejected. A legacy
artifact without the lifecycle capability can be selected only when an already
installed lifecycle-capable helper coordinates the rollback. For a fresh macOS
install, install the current release first rather than using a legacy artifact.
If a process reports retained lifecycle recovery evidence or is killed while a
reservation exists, do not delete the marker or retry another writer. Preserve
the reported workspace and fixed install root, inspect and restore the captured
service/routing state, then have a reviewed current helper release the exact
owner-only marker. This deliberately fail-closed recovery is manual.

For a macOS release workstation, create a dedicated generic-password signing
item with `scripts/bootstrap-keychain-signing-key.sh`. The builder reads it
through the native Security API as raw PEM data; do not use the legacy
`security -w` text conversion for that signing key.

For the documented legacy `pw_opencodex`, `opencodex`, or
`pw_opencodex_remote` root provider, add `--migrate-legacy`. The explicit
migration also accepts only the former direct loopback base URL at
`127.0.0.1:10100` or `localhost:10100`; it saves a timestamped mode-`0600`
backup, removes only the known root assignment and `model_catalog_json`, and
leaves the old provider table for rollback reference:

```bash
./client/relay/scripts/install-relay.sh install REPLACE_WITH_VERSION \
  --release-base-url https://REPLACE_WITH_RELEASE_HOST/opencodex-relay \
  --public-key /secure/path/opencodex-relay-release-ed25519.pub \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --migrate-legacy
```

Any other custom provider is intentionally rejected for manual review. Remove
the relay-owned block with `install-relay.sh uninstall`; restore a retained
legacy backup only after inspecting it and before returning the Remote home to
legacy routing.

Check the result without displaying a secret:

```bash
~/.local/lib/opencodex-relay/relay/current/opencodex-relayctl status
codex debug models
```

The catalog refreshes on the configured interval, filters `visibility: "hide"`,
and atomically preserves the prior file. A new CLI process sees it at startup.
Automatic AppServer restart is disabled by default. It is an explicit Linux
opt-in requiring both `catalog.manage_app_server: true` and an exact absolute
`catalog.app_server_home`; the target AppServer must expose the same
`CODEX_HOME` through Linux `/proc`. Any missing, different, or unverifiable
identity leaves the pending marker intact. macOS deliberately fails closed and
requires a manual CLI/Desktop restart. A manual `relayctl catalog refresh` does
not fetch, write, or activate a relay-owned catalog; it reports that the
resident lifecycle is the sole writer. A `remote_manager` catalog is likewise
reported as owned by that manager.

## Safe Desktop mode switching and connection status (macOS 26+)

The signed MenuBar app controls only a Codex Desktop `.app` whose reviewed
bundle ID, strict signature identifier, and exact Apple Team ID all match, and
it revalidates that identity before every lifecycle boundary. A selected path
is not trust evidence. The production and local-development bundles pin the
`com.openai.codex` / `2DC432GLL2` tuple independently reviewed from an
Apple-notarized OpenAI Codex Desktop installation on 2026-08-23. Builders and
installers reject a missing or changed tuple. If a future Codex signer identity
changes, discovery, quit/relaunch, routing apply, and safe removal fail closed
until a new official signed build is independently reviewed and the tuple is
explicitly updated. Desktop lifecycle never uses AppleScript or force quit.
Native mode removes only the relay-owned configuration block; it does not turn
the relay into an OpenAI proxy.

```text
opencodex-relayctl mode status --json
opencodex-relayctl mode request native|external|local_opencodex|relay --json
opencodex-relayctl mode apply --confirm-desktop-exited --json
opencodex-relayctl mode cancel --json
opencodex-relayctl mode recover --complete|--rollback --confirm-desktop-exited --json
opencodex-relayctl mode repair-native --expected-routing-generation N \
  --confirm-local-development-native-repair --json  # local-development only
```

`request` only records intent. Deprecated `enable` and `disable` spellings are
also request-only aliases; neither mutates the Codex route. The MenuBar app requests a normal quit, waits
for the selected app to exit, then invokes `apply` and relaunches it. Active
requests are drained rather than replayed or handed to another backend. While
native or recovery routing is active, stale relay callers receive a typed 503
and catalog/probe egress is parked. If a local-development state is orphaned in
recovery and both ordinary recovery actions lack evidence, the Control Center
may offer native repair only after an explicit confirmation. The helper requires
the exact displayed generation, no routing/removal journal or gate, exact path
bindings, and independent proof that no Relay, OpenCodex, foreign-owner, or
unmanaged Codex routing artifact exists. It then changes only the routing-state
file to the next native generation; production scope, Codex TOML, OpenCodex,
helper, and service configuration are never modified.

### External ↔ Local OpenCodex (10100) profiles

macOS installs keep `external_gateway` as the canonical/default relay profile.
The Control Center can explicitly select **External gateway**, **Local OpenCodex
(10100)**, or **Native ChatGPT Codex**. Local is disabled until the relay makes
a bounded, credentialless, no-proxy/no-redirect check of
`/healthz` (`service: "opencodex"`, `status: "ok"`, port `10100`) and a
visible, duplicate-free `/v1/models` response. Its catalog has a distinct
relay-owned path; it never shares the External catalog writer.

**Find local OpenCodex installations…** escalates from Tier A enrollment,
canonical absolute launcher, and native-prefix evidence to Tier B trusted npm
and bounded version-manager roots. Tier C is an explicitly approved, bounded
local-volume scan and never grants mutation authority. Discovery schema 4 adds
`teardown_capability`, `data_capability`, a bounded compatibility reason, and
the existing `homebrew_guarded_npm` / `homebrew_guard_required` state. The app
reads schema 2, 3, and 4, but schema 2/3 candidates are display-only. Automatic
removal requires a schema-4 Tier A/B candidate whose npm/Node/Bun/CLI/package
closure is unchanged and whose package identity exactly matches a reviewed
Relay teardown profile. The darwin/arm64 registry is an exact stable allowlist:
`2.22.0`, `2.23.0`, `2.24.0`, `2.24.1`, `2.24.2`, `2.25.0`, `2.26.0`,
`2.27.0`, `2.28.0`, `2.29.0`, `2.31.0`, `2.32.0`, `2.32.1`, and `2.33.0`.
Each profile binds npm integrity, the complete reviewed installation closure,
required-module hashes, and a version-specific adapter ID. Preview releases
and stable `2.30.0` are excluded. Package identity is an explicit profile
registry paired with an adapter implementation registry: a future version is
enabled by adding one reviewed, non-conflicting profile and adapter, not by
weakening discovery or adding a version heuristic. Missing, duplicate, or
mismatched profiles and implementations fail closed. A separate sanitized,
untruncated Tier-B pass must still reproduce the exact installation ID and
fingerprint exactly once; all ambiguous, partial, Tier C, and manual results
remain manual-only.

A standard arm64 Homebrew global npm installation under exactly
`/opt/homebrew` may use `homebrew_guarded_npm` when its current-user ownership
and complete execution evidence are proven and only group-write prevents the
ordinary `exact_npm` check. Production and local-development builds both expose
the generic fixed-purpose `OpenCodexRelayHelperInstaller` command in **App
Information**, with isolated service IDs and CDHashes. The user runs that
command explicitly in Terminal to install the helper at fixed `/Library`
locations. The app never receives the administrator password. Both profiles
record the original
inode/device/mode in a root-owned mode-`0600` journal, removes group-write only
for the operation, and restores it in reverse order. The helper never deletes
OpenCodex, invokes npm, changes ownership, runs a shell, or accepts an arbitrary
path; the existing Go removal helper and removal/routing journals remain the
only deletion authority. World-writable paths, ACLs, foreign ownership,
symlinks, conflicting launchers, incomplete evidence, a missing approval, or
an unresolved protection journal remain fail-closed and manual-only.
Existing fixed system publishing directories must be root-owned regular
directories with `0755` base permissions. A pre-existing sticky bit (`1755`)
is accepted and preserved; setuid, setgid, group/world write, foreign ownership,
and symlinks remain fail-closed.

The Control Center's **App Information** and **Maintenance & Recovery** pages
can start privileged-helper setup before an uninstall candidate is selected.
Production opens **Login Items & Extensions** when approval remains pending.
The ad-hoc development card copies only the reviewed `install`, `update`, or
explicit interrupted-transaction `recover` command and opens Terminal; it
automatically rechecks readiness when the app becomes active again. A
root-owned schema-2 installer journal records only bounded phase and backup
witnesses. Recovery completes the current exact bundle or restores the prior
byte-exact helper/LaunchDaemon and launchd state; malformed or legacy journals
remain fail-closed. A pristine `preparing` transaction may be discarded without
complete backups only when its original file witnesses and launchd state still
match exactly; `backups_ready` and later phases remain backup-bound. Neither
assistance path starts Homebrew protection or package removal.

Before that manual helper is ready, the local installer keeps the exact verified
candidate in an owner-only `pending/<version>` directory and does not modify the
current app, Relay service, or binding. Rerunning the same install command after
the fixed administrator command revalidates the artifact hash, app/helper/
installer CDHashes, and XPC readiness, then promotes only that same candidate.
Installer receipt schema 1 adds optional bounded `failure_phase`,
`failure_reason`, and `rollback_result` fields without paths, UIDs, CDHashes, raw
errors, or child output.

The two exact-executable/fingerprint retain handoffs remain available: keep the
proxy while releasing integration and Shim, or keep the proxy while releasing
integration only. Legacy handoff no longer exposes `ocx uninstall`. Full
removal is a separate opaque installation-ID/fingerprint wizard. Integrated
schema-4 removal is strictly `preserve_only`: it does not call data inventory
or Trash, and it preserves settings, credentials, logs, and every other
OpenCodex data root. There is no caller path/glob/package-name authority,
implicit-all, automatic elevation, or permanent-delete fallback, and the Codex
app is never removed.

Removal has two explicit contexts. A ready routing binding uses the existing
integrated path and freezes the displayed nonzero `UInt64` routing generation.
When the binding is exactly missing, the app may instead use the
`standalone_native` path: it accepts only the standard
`~/.codex/config.toml`, restores and verifies Native Codex configuration plus
`clientIntegrations.codex=false` before package removal, and requires no
server URL, Gateway credential, Relay configuration, LaunchAgent, Keychain
item, or running Relay service. Clean Native or verified OpenCodex-owned
configuration is eligible; custom `CODEX_HOME`, local-Relay, mixed, foreign,
and unmanaged configuration is manual-only. The automatic path is limited to
canonical app-managed roots; ambient `XDG_CONFIG_HOME` or `OPENCODEX_HOME`
overrides are rejected. Unsafe or invalid bindings,
preview mode, partial Relay assets, integration recovery, and conflicting
removal journals remain fail-closed. The standalone path does not create or
modify the listed Relay integration assets.

The integrated context remains `preserve_only`. Standalone Native also defaults
to preserving all data; when an eligible candidate explicitly advertises
`selective_trash_v1`, only items returned by its verified inventory can be
selected and moved to Trash after the existing second confirmation. The Native
boundary and inventory revision are rechecked around that operation. There is
no implicit-all selection or permanent-delete fallback.

Relay owns the versioned teardown adapter and runs it with verified Bun from a
private immutable package snapshot, without a shell, ambient `PATH`, or a path
supplied by the caller. The adapter stops managed service/proxy state, restores
native routing, releases managed integration/environment/shell hooks, and
safely restores the Shim. The internal `relay_preserving_teardown` schema-1
receipt must prove `data_preserved=true`, `config_root_removed=false`, and all
required component postconditions before npm removal can begin. The official
OpenCodex package is not patched and the separate `opencodex/` checkout is not
an execution dependency.

The wizard remains visible during handoff and shows safety preflight, Desktop
exit, the approved OpenCodex action, Desktop relaunch, and context-appropriate
Native or integrated verification as distinct steps. The integrated context
continues to block recovery, applying, unavailable, or unverified routing
before Desktop exit and before any OCX invocation. The standalone context
permits only an exactly missing binding and independently revalidates its Native
boundary before teardown and package execution. After a successful Shim
handoff, status and candidates are refreshed; automatic removal is re-enabled
only when exactly one candidate with the same canonical package root and
executable produces a fresh eligible fingerprint. A missing, duplicate, or
changed candidate remains locked and requires discovery again. After a partial
or unknown result the app still attempts Desktop relaunch and bounded status
verification, displays the confirmed recovery state, and never bypasses the
existing fail-closed removal guard.

Success is receipt-first: package absence, preserved-data proof, and the final
integrated routing or standalone Native boundary must all be proven before
recovery is cleared or the exact trusted Desktop is relaunched. Unverified
process cleanup requires a platform-attested whole-Mac reboot; unresolved
context recovery remains fail-closed. Terminal cleanup independently rechecks
the exact selector, package absence, and its context-bound routing generation or
Native fingerprint, then durably releases the recovery gate while retaining a
replayable terminal witness. After the app validates that receipt, it stores and
reads back a schema-3, context-bearing `terminal_ack_pending`
checkpoint with the exact `terminal_receipt_digest`. It then sends that digest
to `discover-open-codex-native --acknowledge-terminal-receipt-digest`; a bare
discovery or a different digest cannot consume the journal. The acknowledgement
revalidates the same boundary and package absence before retiring that exact
journal and returning `ready/native`. Only then does the app clear and read back
its local checkpoint. A crash before acknowledgement replays the terminal
receipt; a crash after acknowledgement but before local cleanup retries the
idempotent acknowledgement. Neither window opens Relay setup beside an
unresolved checkpoint or journal. Teardown and
package children each arm a durable typed execution witness before launch.
While either witness is active, replay, package resume, and finalization are
blocked. After an attested changed boot,
teardown returns to the durable operation intent without replaying that child,
and package follows its absence/residual branch. Legacy integrated
data-inventory/Trash recovery records are not translated or resumed by either
context; they remain blocked for reviewed manual recovery. New standalone
inventory and Trash witnesses stay bound to their `standalone_native` journal
and Native boundary. Saved reboot/in-flight recovery may also review the same
durable generation while the relay is unreachable, but only through its narrow
saved-session predicate; it
never relaxes the ordinary uninstall predicate. A known pre-start routing
refusal or cleanup-verified malformed receipt is first marked durably, then
routing is parked when required, and only then is the active witness cleared
into a typed retry phase. A gated `generation=0` remains display-only and
non-actionable. UI recovery sessions are versioned and path-free. A
routing-recovery action itself still requires a
healthy relay. Its exact journal gate remains durable through controller
recovery and is cleared only after the same opaque installation selector,
reviewed routing generation, validated routing transaction witness, new stable
route, acknowledged live health, and exact Codex ownership are reverified. A
strictly newer safe recovery generation is checkpointed with the same opaque
selector without taking action and requires another explicit confirmation.
See the canonical [Korean runbook](../../docs/local-codex-relay.ko.md) and the
full [English operations guide](../../docs/local-codex-relay.md) for the exact
preconditions, confirmations, and disposable clean-user/TCC acceptance.

External ↔ Local uses the same Desktop quit → request drain → apply → relaunch
boundary. The relay PID/listeners stay resident, but no request is replayed or
silently redirected. If an active Local backend later fails its identity check,
the relay parks it and returns typed `503 local_opencodex_unavailable`; only an
explicit Desktop-safe External or Native apply can leave that state.

The MenuBar popover summarizes only the selected app and currently applied
route. The sidebar-based Control Center consumes redacted JSON from the bundled
relayctl helper and performs routing, Desktop, Local OpenCodex, maintenance, and
settings actions only through the existing safety gates. It reports local
relay health, routing acknowledgement, catalog state, active work, and the
last bounded remote observation; it never exposes upstream URLs, credentials,
or raw errors. A macOS-installed external gateway may make a bounded ten-minute
connection probe only while `relay_active`. Native Desktop effectiveness remains
`unverifiable` until a live Codex task succeeds.

Toolbar refresh requests are coalesced rather than dropped when a status poll
is already in flight. Only a newly validated status clears the prior status
error. The UI distinguishes a changed snapshot from a current snapshot with no
change; completion logs contain only changed, generation, and phase.

Control Center **Activity Log** keeps up to 500 current-session events and can
copy filtered JSON Lines or a bundle-specific macOS log command. Only allowlisted
status fields are recorded. Routing snapshots mirror the Overview and
Connection & Routing cards; sensitive paths, output, and credentials are excluded.
Clearing the list does not erase the system log.

The app is a regular Dock-visible macOS application with its own `AppIcon.icns`.
Clicking its Dock icon or **Connection details…** brings the same singleton
Control Center forward instead of creating another window. The Control Center
uses native macOS 26 sidebars, toolbars, sheets, and Liquid Glass button styling
without overlaying a custom background material.
Status and explanatory text use the macOS body size and semantic label colors.
Values begin beside a bounded label column, while related buttons stay in one
row and fall back to a vertical stack only at narrow detail widths.

The **Codex Desktop** page also summarizes the exact TOML selected by the
owner-only routing binding. Each check reloads that binding and opens the TOML
with no symlink following, then compares its regular-file identity before and
after inspection. Complete text is revealed only after a privacy warning, is
read-only and selectable, and is cleared when the preview sheet closes. Preview
is limited to 1 MiB of valid UTF-8. Default App, Visual Studio Code, Xcode,
TextEdit, and an application chooser use NSWorkspace only; the file is
revalidated immediately before opening and is not added to recent items. Relay
never edits or saves Codex TOML, and logs only bounded result tokens.

The **App Information** page reports the app version, build, bundle identifier,
distribution flavor, minimum macOS version, and runtime architecture. It also
checks the two fixed bundled Go executables, opencodex-relay and
opencodex-relayctl, using their bounded local version commands. Revision-4
bundles additionally report the privileged Homebrew guard helper version,
registration state, and protocol version. The page does not display or record
executable paths, modes, UIDs, signing hashes, raw output, or process errors.

For **System Default**, the MenuBar UI uses only the first preferred macOS
language: `ko` selects Korean, `en` selects English, and unsupported or missing
languages fall back to Korean. The Control Center Language menu can immediately choose System Default,
Korean, or English. The choice is stored in the app's own bundle-scoped
preferences, so `OpenCodexRelay` and `OpenCodexRelay Dev` never share a
language setting. Relayctl JSON fields and CLI identifiers remain stable
English protocol values.

The macOS installer writes an owner-only, non-secret exact-path binding at
`~/Library/Application Support/OpenCodexRelay/routing-binding.json`. The app
rejects a missing, malformed, symlinked, broad-permission, or foreign-owned
binding rather than falling back to a default Codex home.

Codex AppShot acceptance must run in a newly opened task whose workspace root
is the physical checkout
`/path/to/OpenCodex-OCI-Gateway`. Keep any
`/path/to/IdeaProjects` filesystem symlink intact; remove only the stale
workspace alias in Codex, reopen the physical path, and restart Codex so a new
writable-root profile is created. Prove, in order, that a sandbox command runs,
the singleton Control Center window is discoverable, and an attached AppShot
renders in the conversation. If only the final step fails, investigate Screen
Recording and Accessibility permission separately from Relay window sharing.

## Compatibility surface and feature gates

The local and central allowlists are deliberately identical for:

- models, Responses HTTP/SSE and its WebSocket upgrade, and compact;
- image generations/edits, OpenCodex artifact download, and alpha search;
- GPT-Live/Realtime call setup and sideband WebSocket paths only when enabled.

Voice requires two independent opt-ins: set `voice_enabled: true` in the local
relay JSON **and** run `sudo ./pilot/scripts/configure-gateway-features.sh voice
on` on the central gateway. The central default remains `404`. The API route
does not prove Desktop audio/WebRTC media behavior; validate an actual call
after enabling it. Desktop image-generation controls that use a native product
control plane rather than this OpenAI-compatible `/v1` path stay native and are
not replaced by the relay. Compatible `/v1/images/*` requests are forwarded.

### Opt-in Responses normalizer

`responses.model_modes` can select `bounded_json` for an exact,
case-insensitive model ID. Keys may not contain surrounding whitespace,
duplicate another key by case, or inherit across aliases or colon families.

```json
"responses": {
  "websocket_mode": "http_fallback",
  "model_modes": {
    "opencode-go-responses/gpt-5.6-luna": "bounded_json"
  }
}
```

For the selected `POST /v1/responses`, the relay changes only the top-level
`stream:true` token to `false`, issues exactly one upstream request, strictly
validates the bounded terminal JSON, and returns `response.created`, one
`response.output_item.done` per item, the original terminal status, one
`[DONE]`, and EOF. IDs, function-call payloads, screenshots, usage, and unknown
JSON fields are preserved. Malformed, oversized, timed-out, or unexpectedly
streaming responses fail closed without replay.

Identity and zstd requests are supported. Request spools are mode `0600` and
immediately unlinked; normalization uses at most two concurrent slots. Hosted
image tools remain on their original streaming path. Native-Codex-hosted
`computer` requests fail before dispatch because Codex 0.147 cannot execute a
`computer_call`; ordinary plugin/MCP Computer Use remains a normal
`function_call`. Responses WebSocket handshakes receive `426` in this profile,
while Images, Live, Realtime, Voice, and AppServer transports stay outside the
normalizer.

### General and explicit interactive lanes

The relay keeps the configured general listener (normally
`127.0.0.1:18180`) and a distinct numeric-loopback interactive listener. When
`responses.scheduler.interactive_listen_address` is absent, the latter defaults
to port `18182` in the general listener's IPv4/IPv6 family. Another configured
port remains valid when the relay config validator accepts it.

The installer atomically maintains
`$CODEX_HOME/opencodex-relay-interactive.config.toml`. Apart from its ownership
marker, the file contains exactly these two settings:

```toml
openai_base_url = "http://127.0.0.1:18182/v1"
model_catalog_json = "/ABSOLUTE/CODEX_HOME/opencodex-relay-catalog.json"
```

It never sets `model`, reasoning options, agent limits, or another runtime
policy. General TUI, `exec`, `review`, and daemon invocations remain on the base
listener. Select the reserved lane explicitly:

```bash
codex --profile opencodex-relay-interactive
codex exec --profile opencodex-relay-interactive 'REPLACE_WITH_PROMPT'
```

Both `/__relay/healthz` endpoints identify their `listener_lane`, both listener
addresses, scheduler limits, and nonnegative dynamic counters. Installer and
Remote gates require the general endpoint to report `general`, the interactive
endpoint to report `interactive`, and both static contracts to match
`relay.json` before accepting activation.

## Remote Control Linux home

For a Remote Control host, install the relay using the dedicated home’s catalog
and disable generic AppServer process management. The Remote manager owns its
intentional daemon restart and understands both relay and legacy catalog marker
names:

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

~/.local/lib/opencodex-relay/configure-remote-codex-routing.sh \
  enable-relay --allow-remote-interruption
```

This writes `"routing_mode": "relay"` only after the native configuration is ready,
unsets edge credentials before launching Codex, restarts the managed daemon,
and may disconnect a Remote session. Keep the explicit
`"routing_mode": "legacy"` value until this explicit migration succeeds.

## Non-integrating macOS Preview run

From the repository root, `./script/build_and_run.sh` builds and launches a
complete Preview bundle. The bundle contains the privileged helper and fixed
installer so all Control Center states can be reviewed, but
`OpenCodexRuntimeMode=preview` prevents it from consuming a routing binding or
changing launchd, Keychain, `/Library`, an installed app, or Codex routing.
There is no Terminal launch or automatic privilege elevation.
The relocation card is visible for review, but its file actions are disabled.

`./script/build_and_run.sh --integration-preflight` is read-only. It prints
only bounded state codes: exit 0 is `ready`, exit 3 is
`integration_required`, and exit 4 is `unsafe` or `invalid`. It never prints
the binding, app, configuration, or credential paths. Preview displays the
consumer self-hosted onboarding for review but cannot apply integration or
change the system.

The separate **Source → Package → Gateway → Signing → Review** producer guide is
not part of Settings. It is available only from the Developer menu when a local
development build is launched with `--enable-producer-tools`. It never persists
its inputs, inspects Keychain, reads PEM contents, opens Terminal, or runs a
generated command; only the copied command kind is recorded.

## Local-only development distribution (macOS arm64)

`build-local-dev.sh` and `install-local-dev.sh` are an intentionally separate
path for a person or a small organization directly transferring a reviewed
development bundle. They do **not** relax the revision-4 production builder or
installer: there is no release URL, GitHub download, Developer ID, notarization,
Gatekeeper assessment, quarantine removal, or automatic updater.

The builder requires a clean Git commit, signs the local manifest with Ed25519,
and ad-hoc-signs the nested helpers and app only for structural verification:

```bash
./client/relay/scripts/build-local-dev.sh 1.2.3-dev.1 \
  --signing-key /secure/off-repo/local-dev-ed25519.pem \
  --output /secure/local-transfer/1.2.3-dev.1

./client/relay/scripts/install-local-dev.sh install 1.2.3-dev.1 \
  --source-dir /secure/local-transfer/1.2.3-dev.1 \
  --upstream https://REPLACE_WITH_API_HOSTNAME/v1 \
  --acknowledge-local-source \
  --acknowledge-local-development-source
```

For repeat transfers, pin the separately verified public PEM in the current
user's Keychain, then use `--keychain-service` instead of trusting the PEM in
the source directory. The installer uses separate `relay-dev` paths,
`127.0.0.1:18190/18192`, `io.github.novelkr.opencodex-relay.dev`, and
`OpenCodexRelay Dev.app`; it starts parked and never edits Codex routing
until the MenuBar completes the usual Desktop quit → apply boundary. A
production and dev relay cannot own the same Codex config. This local development
bundle is outside the reviewed release channel and must be manually opened and
approved by its user; login registration is
optional and is never performed by the installer.

The old `--acknowledge-unsigned-local-build` spelling remains a deprecated
compatibility alias only.

## Build and publish a release

The public release authority is `.github/workflows/relay-release.yml`. A
lightweight strict-SemVer tag without a `v` prefix starts the workflow only in
the public `novelKR/OpenCodex-OCI-Gateway` repository. The preflight binds that
tag to current public `main`, requires the successful `linux`, `macos`, and
`analyze` checks for the exact commit, and refuses an existing release or a
tag that is not current public `main`. Because the job token cannot read the
repository's administrative immutable-release setting, the operator enables
and verifies it before tagging; the publisher then verifies the released
version's immutable state. Any failure after draft creation triggers a bounded
deletion attempt; if GitHub has already made that release immutable and rejects
cleanup, CI reports the unresolved public state and client enrollment must stop.
Tags with a SemVer suffix, such as `0.3.8-rc.2`, are additionally required to
publish and read back as GitHub pre-releases. Every new tag must add a bounded
version-specific note at `client/relay/release-notes/VERSION.md`; CI embeds that
reviewed fragment ahead of the common installation and verification guidance.

The protected `relay-release` GitHub Environment holds the base64-encoded v2
Ed25519 private key. The macOS job compares its derived public key with
[`../../config/trust/opencodex-relay-release-ed25519.pub`](../../config/trust/opencodex-relay-release-ed25519.pub),
builds and independently verifies exactly eight assets, attaches GitHub build
provenance, and then calls `scripts/publish-github-release.sh`. The publisher
creates a draft, verifies its exact asset set and reviewed body, publishes it,
and attempts bounded cleanup if the final immutable state cannot be confirmed.
Release `0.3.6` is the first version using this public CI path.

Do not commit the private key, its base64 representation, a generated release
directory, credentials, or live request logs. Manifest compatibility revision 4 signs component identity,
the macOS bundle ID, `signing_mode: "adhoc"`, the final app zip hash, and the
notice URL/SHA-256. The installer verifies the Ed25519 signature, every asset
hash, the nested ad-hoc signatures, the absence of a Relay Team ID, and the
Hardened Runtime before selecting the bundle. Revision 1 and 2 releases remain
eligible rollback targets only when an already installed lifecycle-capable
helper coordinates the macOS transaction; they are not supported as a fresh
macOS install through the current script. They predate the parked routing
controller and must not be used for a MenuBar-managed Desktop switch; close
Desktop before using their legacy compatibility path. The app includes
`OpenCodexRelayHelperInstaller`; its `install|update|uninstall|recover|status`
commands require a separate administrator approval and bind the exact app,
installer, and helper CDHashes. A busy or recovery-required protection journal
blocks app replacement. Retain the
previous release directory so changing the `current` symlink can be rolled back
by reinstalling the prior reviewed version.

Install the exact public release with `--github-repo
novelKR/OpenCodex-OCI-Gateway` and the tracked public key. A mode-`0600` token
is optional when anonymous API rate limits are insufficient; details and the
Remote-host command are in [`../../docs/local-codex-relay.md`](../../docs/local-codex-relay.md).

The macOS app is deliberately not notarized and has no Apple publisher
identity. Open it from Finder once. If macOS blocks it, immediately open
**System Settings → Privacy & Security** and choose **Open Anyway** for
OpenCodexRelay (the button is normally available for about one hour after the
blocked launch), then confirm **Open**. A rebuilt or updated app may require the
same approval again. Do not remove quarantine attributes or disable Gatekeeper.

### Ownership-based local-development native repair

`inspect-native-repair --expected-routing-generation N --json` exposes no URL,
path, or TOML value. It reports only `state_only`, `local_relay`, `opencodex`,
or `unavailable` plus presence booleans for the two managed keys. Foreign,
mixed, incomplete, and unmarked user overrides remain manual-only.
`repair-native-routing` rechecks the exact generation, Desktop-exited
confirmation, absent journals/gates, and physical path binding. It removes only
the current Relay marker block and managed profile. OpenCodex restore authority
is independent from package-removal authority: discovery binds an optional
native-restore fingerprint to the unchanged installation ID/fingerprint, then
the helper re-discovers the exact Tier A/B candidate and executes only a private
snapshot of its bundled Bun, CLI, and package tree with a sanitized environment.
Before Desktop exits, a read-only `inspect-native-repair-owner`
preflight returns only configuration validity and Codex integration intent; invalid or
unavailable results keep repair disabled without quitting Desktop. A path-free timestamped mode-`0600` backup precedes mutation.
Generation advances only after native routing validates. If TOML is native but
state commit fails, the old recovery generation remains and state-only repair is
the next explicit action. OpenCodex Codex integration is disabled; its Shim,
proxy, package, and data remain installed. Only a proven no-mutation desired-state
conflict is retried three additional times at 200/500/1000 ms. Busy exhaustion, invalid
configuration, restore failure, and invalid bounded output remain separate safe error codes.
